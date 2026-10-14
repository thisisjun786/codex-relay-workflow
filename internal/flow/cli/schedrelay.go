package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"
)

// SchedRelay is the relay's DAG scheduler as the project lane of a run uses it (ops.Relay over internal/relay/dagsched, CRW-1277). The
// run is the orchestrator of its plan: it claims the plan's coordinator epoch before its first fenced write and holds it for as long as it
// lives, releases the issue nodes to agents of the run (dag-release --surface agent), accepts and integrates them on the agent path with
// the same scheduler and the same integration wiring as `crw relay dag-integrate`, and closes the executions it gives up. The milestone
// it works on is the compiled milestone of the crw-plan handoff the run pinned: the nodes of the plan at the revision the run pinned,
// the criteria, regions and group of their issues from the handoff, and the title, criteria, surfaces, grade and required commands of
// the milestone's entry.
type SchedRelay struct {
	Sched *dagsched.Scheduler
	// RunID is the run; Actor the task that runs it (the registered parent of the plan's project); Nonce the session nonce of its epoch
	// claim (the run id: a resumed process is the same session, a new run is another).
	RunID, Actor, Nonce string
	// Revision is the plan revision the run pinned (journal.Identity.PlanRevision).
	Revision int64
	// Handoff is the handoff the run pinned; State the relay state directory (the run's artifacts are under <state>/flow/<run>); Repo
	// the repository the run's worktrees are made from, which the plan's target has to be ("" checks nothing).
	Handoff HandoffRef
	State   string
	Repo    string
	// CreatedAt is the time the run was started at: the capture time of the pinned handoff.
	CreatedAt string
	// Journal is the run's journal: the worktree a node's work is in and the acceptances the run made are read from it.
	Journal engine.Journal
	// Rule is the rule version a release is made under (dag-release-request/1 rule_version) and Verifier the pair an acceptance names
	// as the one it was verified under.
	Rule     dagsched.RuleVersion
	Verifier dagsched.VerifierRule
	// Verify is the verification an integration batch runs over the merged tree.
	Verify dagsched.IntegrationBatchVerifier
	// HostMemory samples the host memory bound for a release (dag-release applies it, read once per command; the run lives long, so it
	// is sampled for each release); nil applies none. A threshold that is not usable is an error of the release.
	HostMemory func() (*dagsched.HostMemoryBound, error)
	// RunnableCommand says why the run's verifier cannot run a required command of a milestone ("" when it can); nil checks nothing.
	RunnableCommand func(command string) string
	// Child is the host settings of a managed child start (SP-CHILD-1, CRW-1285): the crw-flow-child-host/1 document the run pinned
	// (--child-host); nil when it pinned none, and a node sized for a child is then refused with the reason. The scheduler's Start and
	// Selectors are the managed engine the child is started with.
	Child *ChildHost

	// release serializes the releases: the bound of the host is set on the scheduler for the one being made.
	release sync.Mutex
	mu      sync.Mutex
	claimed string
	input   *compile.Input
	// testAfterBatch is a test seam: it runs after the integration batch and before its observation, and an error it answers is the
	// integration's.
	testAfterBatch func() error
}

var _ ops.Relay = (*SchedRelay)(nil)

func (r *SchedRelay) q(ctx context.Context) store.Querier { return r.Sched.Store.Q(ctx) }

// handoff is the run's pinned handoff, read once and checked against the sha256 the run fixed.
func (r *SchedRelay) handoff() (compile.Input, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.input != nil {
		return *r.input, nil
	}
	if r.Handoff.Path == "" {
		return compile.Input{}, errors.New("the run names no compiled milestone: it was started without --handoff, so there is no handoff to read the milestone from")
	}
	_, in, err := ReadHandoff(r.Handoff)
	if err != nil {
		return compile.Input{}, err
	}
	r.input = &in
	return in, nil
}

// project is the project of the plan in the pinned handoff.
func (r *SchedRelay) project(plan string) (compile.Project, error) {
	in, err := r.handoff()
	if err != nil {
		return compile.Project{}, err
	}
	p, ok := HandoffProject(in, plan)
	if !ok {
		return compile.Project{}, fmt.Errorf("the run's handoff holds no project of plan %s: compile and put the handoff first", plan)
	}
	return p, nil
}

// target is where the plan's nodes integrate: the local checkout and the branch the handoff's target names. The agent path integrates
// locally (the pull request landing of an agent node is CRW-1226's), so a target that is not a local checkout is refused.
func (r *SchedRelay) target(plan string) (checkout, ref string, err error) {
	p, err := r.project(plan)
	if err != nil {
		return "", "", err
	}
	if p.Target == nil || p.Target.Repository == "" || p.Target.Branch() == "" {
		return "", "", fmt.Errorf("the project %s of the run's handoff names no target {repository, base_ref}: the run integrates its nodes there", p.Key)
	}
	if !filepath.IsAbs(p.Target.Repository) {
		return "", "", fmt.Errorf("the target repository %q of project %s is not a local checkout: an agent node integrates on a local branch (its pull request landing is CRW-1226's)", p.Target.Repository, p.Key)
	}
	if r.Repo != "" && !sameDirectory(r.Repo, p.Target.Repository) {
		return "", "", fmt.Errorf("the target repository %s of project %s is not the repository %s the run's worktrees are made from: the relay reads a node's commits in the target (start the run with --repo %s)", p.Target.Repository, p.Key, r.Repo, p.Target.Repository)
	}
	// the short branch name, as compile put it in the plan's edges: the integration path adds refs/heads/ itself (CRW-1317)
	return p.Target.Repository, p.Target.Branch(), nil
}

// claim takes the plan's coordinator epoch for the run, once, before its first fenced write (CRW-1277 e): the claim of the run's nonce
// under the task that runs it, and the scheduler's expected epoch from then on. A resumed process presents the same nonce and is the same
// claim while no other session took the plan; once one did, the claim is refused and the run stops at the write.
func (r *SchedRelay) claim(ctx context.Context, plan string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.claimed == plan {
		return nil
	}
	if r.claimed != "" {
		return fmt.Errorf("the run holds the coordinator epoch of plan %s and is asked to write plan %s: one run orchestrates one plan", r.claimed, plan)
	}
	res, err := r.Sched.ClaimEpoch(ctx, plan, dagsched.ClaimInput{Actor: r.Actor, SessionNonce: r.Nonce})
	if err != nil {
		return epochLost(plan, fmt.Errorf("the run cannot claim the coordinator epoch of plan %s: %w", plan, err))
	}
	r.Sched.ExpectedEpoch = res.Epoch
	r.claimed = plan
	return nil
}

// ErrEpochLost is the error of a fenced write the run made after another session claimed the plan: the run stops there, and its result
// names the reason. It is the lane's error kind (ops.ErrEpochLost, the closed reason epoch_lost), so it reaches the op's result and the
// script as itself, never as another refusal (CRW-1285).
var ErrEpochLost = ops.ErrEpochLost

// epochLost wraps a stale_coordinator_epoch refusal as ErrEpochLost; any other error is returned as it is.
func epochLost(plan string, err error) error {
	var refused *store.RefusedError
	if errors.As(err, &refused) && refused.Reason == string(contract.RefusalStaleCoordinatorEpoch) {
		return fmt.Errorf("%w %s (another session claimed it): the run stops: %v", ErrEpochLost, plan, err)
	}
	return err
}

// ---- the milestone ----

// Milestone reads the compiled milestone (CRW-1277 c): the nodes of the plan at the run's pinned revision that carry an issue of the
// milestone in the handoff, in the order the scheduler offers them, each with the edges from the milestone's nodes it waits for and the
// criteria, regions and group of its issue; and the milestone's own title, criteria, surfaces, grade and required commands from its
// entry. A handoff that is not the one the pinned revision was compiled from (an issue with no node, or criteria whose digest is not the
// node's) is refused, and so is a milestone with no entry or an entry without surfaces or grade: nothing is defaulted.
func (r *SchedRelay) Milestone(ctx context.Context, plan, milestone string) (ops.Milestone, error) {
	p, err := r.project(plan)
	if err != nil {
		return ops.Milestone{}, err
	}
	entry, ok := HandoffMilestone(p, milestone)
	if !ok {
		return ops.Milestone{}, fmt.Errorf("the run's handoff names no entry of milestone %s in project %s: the milestone's title, criteria, surfaces, grade and required commands are compiled with the plan (milestones[] of %s)", milestone, p.Key, compile.InputSchema)
	}
	if len(entry.Surfaces) == 0 || strings.TrimSpace(entry.Grade) == "" || len(entry.Criteria) == 0 {
		return ops.Milestone{}, fmt.Errorf("the entry of milestone %s names no surfaces, grade or criteria: the run defaults none of them", milestone)
	}
	// the compiler's rules for an entry hold for the handoff the run pinned, however it came to be edited since it was compiled
	if why := EntryProblem(p, entry); why != "" {
		return ops.Milestone{}, errors.New(why)
	}
	if r.RunnableCommand != nil {
		for _, c := range entry.RequiredCommands {
			if why := r.RunnableCommand(c); why != "" {
				return ops.Milestone{}, fmt.Errorf("milestone %s requires the command %q, which the run cannot verify: %s", milestone, c, why)
			}
		}
	}
	if r.Revision <= 0 {
		return ops.Milestone{}, fmt.Errorf("the run pins no revision of plan %s", plan)
	}
	snap, _, err := (&dag.Repo{Store: r.Sched.Store}).Snapshot(ctx, plan, r.Revision)
	if err != nil {
		return ops.Milestone{}, fmt.Errorf("plan %s at the run's revision %d: %w", plan, r.Revision, err)
	}
	byIssue := map[string]dag.SnapNode{}
	for _, n := range snap.Nodes {
		byIssue[n.IssueKey] = n
	}
	issues := map[string]compile.Issue{}
	members := map[string]bool{}
	for _, is := range p.Issues {
		if is.Milestone != milestone {
			continue
		}
		n, ok := byIssue[is.Key]
		if !ok {
			return ops.Milestone{}, fmt.Errorf("issue %s of milestone %s has no node in plan %s at revision %d: compile and put the handoff first", is.Key, milestone, plan, r.Revision)
		}
		if len(is.Criteria) == 0 {
			return ops.Milestone{}, fmt.Errorf("issue %s names its criteria by digest only: the run releases a node with the text of its criteria, so the handoff names them", is.Key)
		}
		if want := compile.CriteriaDigest(is.Criteria); want != n.CriteriaSetDigest {
			return ops.Milestone{}, fmt.Errorf("the criteria of issue %s digest to %s and node %s of plan %s carries %s at revision %d: compile and put the handoff first", is.Key, want, n.NodeID, plan, n.CriteriaSetDigest, r.Revision)
		}
		issues[n.NodeID] = is
		members[n.NodeID] = true
	}
	if len(members) == 0 {
		return ops.Milestone{}, fmt.Errorf("no issue of the handoff is in milestone %s", milestone)
	}
	after := map[string][]string{}
	for _, e := range snap.Edges {
		if members[e.FromNodeID] && members[e.ToNodeID] && !slices.Contains(after[e.ToNodeID], e.FromNodeID) {
			after[e.ToNodeID] = append(after[e.ToNodeID], e.FromNodeID)
		}
	}
	nodes := map[string]dag.SnapNode{}
	for _, n := range snap.Nodes {
		nodes[n.NodeID] = n
	}
	out := ops.Milestone{ID: milestone, Plan: plan, Title: entry.Title, Criteria: slices.Clone(entry.Criteria), CriteriaDigest: compile.CriteriaDigest(entry.Criteria),
		Surfaces: slices.Clone(entry.Surfaces), Grade: entry.Grade, RequiredCommands: append([]string{}, entry.RequiredCommands...), Nodes: []ops.PlanNode{}}
	for _, id := range dagsched.OfferingOrder(snap) {
		if !members[id] {
			continue
		}
		n, is := nodes[id], issues[id]
		pn := ops.PlanNode{ID: n.NodeID, Issue: n.IssueKey, Title: n.Title, After: after[id], Regions: []string{}, Group: is.Group, Criteria: slices.Clone(is.Criteria),
			CriteriaDigest: n.CriteriaSetDigest, SliceDigest: n.SliceDigest}
		sort.Strings(pn.After)
		if pn.After == nil {
			pn.After = []string{}
		}
		for _, rg := range is.Regions {
			pn.Regions = append(pn.Regions, rg.String())
		}
		// the commands of the issue's done condition, read from the body the handoff carries (CRW-1382): the design-stage explorer runs them
		if strings.TrimSpace(is.Description) != "" {
			if v, err := skill.CheckIssueReady(skill.ReadyIssue{ID: is.Key, Title: is.Title, Description: is.Description, Labels: is.Labels}); err == nil {
				pn.EndCondition = v.EndCondition
			}
		}
		out.Nodes = append(out.Nodes, pn)
	}
	return out, nil
}

// issueOf is the issue of the handoff a node of the pinned revision carries.
func (r *SchedRelay) issueOf(ctx context.Context, plan, node string) (dag.SnapNode, compile.Issue, error) {
	p, err := r.project(plan)
	if err != nil {
		return dag.SnapNode{}, compile.Issue{}, err
	}
	snap, _, err := (&dag.Repo{Store: r.Sched.Store}).Snapshot(ctx, plan, r.Revision)
	if err != nil {
		return dag.SnapNode{}, compile.Issue{}, err
	}
	for _, n := range snap.Nodes {
		if n.NodeID != node {
			continue
		}
		for _, is := range p.Issues {
			if is.Key == n.IssueKey {
				return n, is, nil
			}
		}
		return n, compile.Issue{}, fmt.Errorf("node %s carries issue %s, which the run's handoff does not hold: compile and put the handoff first", node, n.IssueKey)
	}
	return dag.SnapNode{}, compile.Issue{}, fmt.Errorf("plan %s has no node %s at the run's revision %d", plan, node, r.Revision)
}

// ---- the release ----

// worktreeHandle is the path the agent execution of a node records as its worktree: a link the run points at the worktree the node's
// work is in before it asks for an acceptance, so the relay reads the head of the node's work from git whatever attempt worktree the
// lane built it in.
func (r *SchedRelay) worktreeHandle(node string) string {
	return filepath.Join(RunDir(r.State, r.RunID), "node-wt", pathElement(node))
}

// sameDirectory says whether two paths name one directory.
func sameDirectory(a, b string) bool {
	if a == b {
		return true
	}
	ia, errA := os.Stat(a)
	ib, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(ia, ib)
}

// pathElement makes a node id one safe path element.
func pathElement(s string) string {
	s = strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c == 0 {
			return '_'
		}
		return c
	}, s)
	if s == "" || s == "." || s == ".." {
		return "_" + s
	}
	return s
}

// Release releases a node to an executor of the run (CRW-1277 d): dagsched.ReleaseAgent under the task that runs it, with the request
// the run holds the inputs of: the base from the plan's target, the rule version, the criteria of the node's issue as c1..cn required,
// the pinned handoff as the volatile input and the run's directory as the artifact root. A node the scheduler holds back (no free slot,
// an overlapping region, a host short of memory, a node that is not ready) is Released false with the reason; a malformed request and a
// fenced write are errors. The request carries the node's slice the flow compared (CRW-1288): the relay compares it with the node's slice
// of the plan's head when it judges the release and again in the transaction that records it, and a slice that is another releases nothing
// (Released false, plan_changed), and so does the replay of a release committed under another slice. A release answers the slice of the
// manifest it committed.
func (r *SchedRelay) Release(ctx context.Context, req ops.ReleaseRequest) (ops.ReleaseResult, error) {
	if req.Land {
		// a land of a node another run holds is answered before the claim: the claim would supersede that run's session (CRW-1354)
		if run, execution, err := r.heldByAnotherRun(ctx, req.Plan, req.Node); err != nil || run != "" {
			return ops.ReleaseResult{Reason: ops.ReasonLandNodeHeld, HeldRun: run, HeldExecution: execution, HeldRunState: r.runState(ctx, run)}, err
		}
	}
	if err := r.claim(ctx, req.Plan); err != nil {
		return ops.ReleaseResult{}, err
	}
	n, is, err := r.issueOf(ctx, req.Plan, req.Node)
	if err != nil {
		return ops.ReleaseResult{}, err
	}
	request, err := r.releaseRequest(req.Plan, n, is)
	if err != nil {
		return ops.ReleaseResult{}, err
	}
	request.ExpectedSliceDigest = req.SliceDigest
	r.release.Lock()
	defer r.release.Unlock()
	done, err := r.hostBound()
	if err != nil {
		return ops.ReleaseResult{}, err
	}
	defer done()
	// the node's admission regions are declared before the relay judges its release (CRW-1391); a declaration that fails is the release's
	// warning, and the release goes on as for an undeclared node (fail open)
	var declared, warning string
	if len(req.Regions) > 0 && request.Base != nil {
		declared, warning = r.declareRegions(ctx, req.Plan, req.Node, request.Base.Repository, is, req.Regions)
		// the milestone's other admitted nodes are declared too: the judgment reads an earlier-ranked ready node that has no declaration as
		// overlapping every other, so a lane released before the one of an earlier node would be held back by it. A failure is that node's
		// own release's to warn of.
		others := make([]string, 0, len(req.Admitted))
		for other := range req.Admitted {
			others = append(others, other)
		}
		sort.Strings(others)
		for _, other := range others {
			if other == req.Node || len(req.Admitted[other]) == 0 {
				continue
			}
			if on, ois, err := r.issueOf(ctx, req.Plan, other); err == nil && on.Kind == dag.NodeImplementation {
				r.declareRegions(ctx, req.Plan, other, request.Base.Repository, ois, req.Admitted[other])
			}
		}
	}
	// a head made outside a run is released with the pair that wrote it, read back from its author thread (CRW-1354): the execution records it
	res, err := r.Sched.ReleaseAgent(ctx, req.Plan, req.Node, r.Actor, request, dagsched.AgentReleaseOptions{Role: "executor", Tier: "executor", RunID: r.RunID, Worktree: r.worktreeHandle(req.Node),
		AuthorModel: req.AuthorModel, AuthorEffort: req.AuthorEffort})
	if err != nil {
		if reason, held := holdBack(err); held && reason == ops.ReasonPlanChanged {
			out, err := r.planChanged(ctx, req.Plan, req.Node)
			out.RegionsDeclared, out.RegionsWarning = declared, warning
			return out, err
		} else if held {
			return ops.ReleaseResult{Reason: reason, RegionsDeclared: declared, RegionsWarning: warning}, nil
		}
		return ops.ReleaseResult{}, epochLost(req.Plan, err)
	}
	body, found, err := dag.ReadManifestOn(ctx, r.q(ctx), res.ManifestDigest)
	if err != nil || !found {
		return ops.ReleaseResult{}, errors.Join(err, fmt.Errorf("node %s was released under manifest %s, which the relay does not read back", req.Node, res.ManifestDigest))
	}
	slice, _ := body["node_slice_digest"].(string)
	run := r.RunID
	if res.Replayed {
		// the run of the execution the replay answers: this run's own on a resume, another run's when that run holds the node
		run = ""
		if cur, found, err := dagsched.CurrentExecution(ctx, r.q(ctx), req.Plan, req.Node); err != nil {
			return ops.ReleaseResult{}, err
		} else if found && cur.Kind == dagsched.ExecutionAgent && cur.Agent.ExecutionID == res.ExecutionID {
			run = cur.Agent.RunID
		}
	}
	// a replay of another run's execution carries that run's state, as a land the relay answers land_node_held does (CRW-1404): a land
	// held by a run that has ended names its cancel
	var state string
	if run != "" && run != r.RunID {
		state = r.runState(ctx, run)
	}
	return ops.ReleaseResult{Released: true, ExecutionID: res.ExecutionID, SlotID: res.SlotID, Replayed: res.Replayed, SliceDigest: slice, Run: run,
		HeldRunState: state, RegionsDeclared: declared, RegionsWarning: warning}, nil
}

// The answers of declareRegions (ops.ReleaseResult.RegionsDeclared).
const (
	regionsDeclared = "declared"
	regionsKept     = "kept"
	regionsFailed   = "failed"
)

// declareRegions declares a node's admission regions in the target repository (dag-region-declare, dagsched.DeclareRegions) when the
// node has no declaration in force (CRW-1391). A node with one keeps it: the parent's graded declaration made before the run (crw-run,
// "Release by region grade") or one an earlier release of the node made, which a resume does not widen. A region that is one of the
// plan's (the handoff's issue regions), in any spelling of it, is declared as the plan states it (kind, symbol, change, exclusive); an edit
// region of the DESIGN is a path, a tree when it ends in "/" and a symbol when it names one after "#". It answers what it did and, for a declaration that
// failed, why: the release goes on.
func (r *SchedRelay) declareRegions(ctx context.Context, plan, node, repository string, is compile.Issue, regions []string) (string, string) {
	if _, found, err := r.Sched.NodeRegions(ctx, plan, node); err != nil {
		return regionsFailed, "the node's declaration in force cannot be read: " + err.Error()
	} else if found {
		return regionsKept, ""
	}
	// a region is looked up and declared once by its normal form (ops.RegionOf, CRW-1404): another spelling of a planned region (blanks
	// around it, ./path) is that region, with the plan's change and exclusive flag, and not a second region of the same place that the
	// relay refuses the whole declaration for. A planned region's normal form is taken from its fields (ops.RegionFormOf, CRW-1408), and
	// the plan's own text of it (the manifest's, RegionForm.Text) is that region and is not read again, so a planned path that holds "#" stays
	// the file or tree the plan states; only a DESIGN edit region is read from its text
	planned := map[ops.RegionForm]compile.Region{}
	spelled := map[string]ops.RegionForm{}
	for _, rg := range is.Regions {
		form := ops.RegionFormOf(rg.Path, rg.Kind, rg.Key)
		planned[form] = rg
		spelled[form.Text()] = form
	}
	list := make([]dagsched.Region, 0, len(regions))
	seen := map[ops.RegionForm]bool{}
	for _, text := range regions {
		form, ok := spelled[strings.TrimSpace(text)]
		if !ok {
			form = ops.RegionOf(text)
		}
		if seen[form] {
			continue
		}
		seen[form] = true
		region := dagsched.Region{Repository: repository, Path: form.Path, Kind: form.Kind, Key: form.Key}
		if rg, ok := planned[form]; ok {
			region.Change, region.Exclusive = rg.Change, rg.Exclusive
		}
		list = append(list, region)
	}
	if _, err := r.Sched.DeclareRegions(ctx, plan, node, r.Actor, list); err != nil {
		return regionsFailed, err.Error()
	}
	return regionsDeclared, ""
}

// runState is the state of a run as `crw flow ps` reads it (its journal and its lock), "" when it cannot be read (CRW-1397).
func (r *SchedRelay) runState(ctx context.Context, run string) string {
	if run == "" {
		return ""
	}
	events, found, err := journal.LoadLean(ctx, r.Sched.Store, run)
	if err != nil || !found {
		return ""
	}
	return Summarize(RunRecord{RunID: run}, events, r.State != "" && LockHeld(r.State, run), 0).State
}

// heldByAnotherRun is the run and the execution of the node's current agent execution when a run other than this one holds it ("" when
// none does, or when this run does).
func (r *SchedRelay) heldByAnotherRun(ctx context.Context, plan, node string) (run, execution string, err error) {
	cur, found, err := dagsched.CurrentExecution(ctx, r.q(ctx), plan, node)
	if err != nil || !found || cur.Kind != dagsched.ExecutionAgent || cur.Agent.RunID == r.RunID {
		return "", "", err
	}
	return cur.Agent.RunID, cur.Agent.ExecutionID, nil
}

// releaseRequest is the dag-release-request/1 the run holds the inputs of for a node: the rule version, the criteria of the node's issue
// as c1..cn required, the pinned handoff as the volatile input and the run's directory as the artifact root, and, for an implementation
// node, the base from the plan's target.
func (r *SchedRelay) releaseRequest(plan string, n dag.SnapNode, is compile.Issue) (dagsched.ReleaseRequest, error) {
	criteria := make([]dagsched.Criterion, len(is.Criteria))
	for i, c := range is.Criteria {
		criteria[i] = dagsched.Criterion{ID: fmt.Sprintf("c%d", i+1), Title: strings.TrimSpace(c), Required: true}
	}
	request := dagsched.ReleaseRequest{Schema: dagsched.SchemaReleaseRequest, RuleVersion: r.Rule, Criteria: criteria,
		CriteriaSource: compile.InputSchema + "@" + r.Handoff.SHA256,
		Volatile:       []dagsched.Volatile{{Source: compile.InputSchema, SnapshotURI: r.Handoff.Path, SHA256: r.Handoff.SHA256, CapturedAt: r.CreatedAt}},
		ArtifactRoots:  []string{RunDir(r.State, r.RunID)}}
	if n.Kind == dag.NodeImplementation {
		checkout, ref, err := r.target(plan)
		if err != nil {
			return dagsched.ReleaseRequest{}, err
		}
		request.Base = &dagsched.BaseSpec{Repository: checkout, Ref: ref}
	}
	return request, nil
}

// hostBound sets the host memory bound of a release on the scheduler (the caller holds the release lock) and answers what takes it
// back.
func (r *SchedRelay) hostBound() (func(), error) {
	if r.HostMemory == nil {
		return func() {}, nil
	}
	bound, err := r.HostMemory()
	if err != nil {
		return nil, err
	}
	r.Sched.Host = bound
	return func() { r.Sched.Host = nil }, nil
}

// planChanged is the answer to a release the relay refused plan_revision_conflict (CRW-1291): the revision the run pinned, the plan's
// head revision, and the node's current agent execution the relay holds with the run it was released to (the release a refused replay
// would have answered), so the run-level hold can return its slot or record whose it is.
func (r *SchedRelay) planChanged(ctx context.Context, plan, node string) (ops.ReleaseResult, error) {
	q := r.q(ctx)
	_, head, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return ops.ReleaseResult{}, err
	}
	out := ops.ReleaseResult{Reason: ops.ReasonPlanChanged, PinnedRevision: r.Revision, HeadRevision: head}
	cur, found, err := dagsched.CurrentExecution(ctx, q, plan, node)
	if err != nil {
		return ops.ReleaseResult{}, err
	}
	if found && cur.Kind == dagsched.ExecutionAgent {
		out.HeldExecution, out.HeldRun = cur.Agent.ExecutionID, cur.Agent.RunID
	}
	return out, nil
}

// notReady is the detail of the refusal of a node the reading did not find ready: "<node> is not ready (<reason>): ...".
var (
	notReady   = regexp.MustCompile(`is not ready \(([^()]+)\)`)
	deferredBy = regexp.MustCompile(`\((defer:[a-z_]+)\)`)
)

// holdBack says whether a refused release is the scheduler holding the node back, and the reason: capacity (no slot, a ceiling nobody
// measured, a host short of memory), an overlapping region, or a reading that does not find the node ready now. Those go away by
// themselves. A plan whose head gives the node another slice than the one the flow compared (plan_revision_conflict) is plan_changed:
// the flow reads the plan again. Every other refusal is the request's or the run's fault.
func holdBack(err error) (string, bool) {
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		return "", false
	}
	switch contract.RefusalReason(refused.Reason) {
	case contract.RefusalCapacityExhausted, contract.RefusalCapacityUnmeasured, contract.RefusalRegionOverlap:
		if m := deferredBy.FindStringSubmatch(refused.Detail); m != nil {
			return m[1], true
		}
		return refused.Reason, true
	case contract.RefusalDispositionConflict:
		if m := notReady.FindStringSubmatch(refused.Detail); m != nil {
			return m[1], true
		}
	case contract.RefusalPlanRevisionConflict:
		return ops.ReasonPlanChanged, true
	}
	return "", false
}

// ReleaseStands is true while the node's current agent execution is the release's, and for an execution whose acceptance integrated.
func (r *SchedRelay) ReleaseStands(ctx context.Context, plan, node, execution string) (bool, error) {
	if execution == "" {
		return false, nil
	}
	q := r.q(ctx)
	cur, found, err := dagsched.CurrentExecution(ctx, q, plan, node)
	if err != nil {
		return false, err
	}
	if found && cur.Kind == dagsched.ExecutionAgent && cur.Agent.ExecutionID == execution {
		return true, nil
	}
	acc, found, err := dagsched.CurrentAcceptance(ctx, q, plan, node)
	if err != nil || !found || acc.Kind != dagsched.AcceptanceAgent || acc.Agent.ExecutionID != execution {
		return false, err
	}
	return r.Sched.AgentAcceptanceIntegrated(ctx, q, acc.Agent)
}

// ReleaseChild releases a node the architect sized for a child as an issue child (SP-CHILD-1, CRW-1285): dagsched.Release, the managed
// start of dag-release, under the task that runs the run, with the request the run holds the inputs of (as Release: the base from the
// plan's target, the rule version, the criteria of the node's issue, the pinned handoff, the run's directory) and the host settings of
// the run's --child-host document: the parent's and the child's host and settings, and the child's title (the managed start prefixes the
// node's issue key). The child it admitted, bound to the node, is the answer. A run that holds no host settings, or settings that cannot start a child, is
// refused with the reason (an input error); a start the managed engine refused or left incomplete, a node the scheduler holds back and
// every other failure are errors. The one ChildResult with Started false is the start under a slice of the plan that is not the node's
// any more (Reason plan_changed, CRW-1382): the request carries the slice crw.release compared, the managed release compares it, and
// crw.releaseChild records that no child started and the lane ends the node unreleased. The intent and the slot of a start that did not
// finish stay, and the same release continues it.
func (r *SchedRelay) ReleaseChild(ctx context.Context, req ops.ChildRequest) (ops.ChildResult, error) {
	if r.Child == nil {
		return ops.ChildResult{}, ErrChildHostUnset
	}
	if why := r.Child.Problem(); why != "" {
		return ops.ChildResult{}, fmt.Errorf("refused:child_host_settings: %s", why)
	}
	if r.Sched.Start == nil {
		return ops.ChildResult{}, errors.New("refused:child_start_unwired: the run's scheduler has no managed-start engine")
	}
	if err := r.claim(ctx, req.Plan); err != nil {
		return ops.ChildResult{}, err
	}
	n, is, err := r.issueOf(ctx, req.Plan, req.Node)
	if err != nil {
		return ops.ChildResult{}, err
	}
	request, err := r.releaseRequest(req.Plan, n, is)
	if err != nil {
		return ops.ChildResult{}, err
	}
	request.Instructions = fmt.Sprintf("You are the issue child of node %s (issue %s) of DAG plan %s, released by flow run %s under design revision %s (%s; sized for a child: %s). "+
		"Run the issue-lane workflow for this node with the plan the run released it with; you coordinate only, and the executor calls of your run author every commit (SP-CHILD-1).",
		req.Node, n.IssueKey, req.Plan, r.RunID, req.Revision, req.DesignDigest, req.Reason)
	request.ScopeRef = "crw-flow:" + r.RunID + "/" + req.Node
	request.Parent = r.Child.Parent
	request.Child = r.Child.Child
	// the node's slice crw.release compared: the managed release compares it when it judges the start and in the transaction that records
	// it, as Release does (CRW-1288, CRW-1382)
	request.ExpectedSliceDigest = req.SliceDigest
	// the managed start sends the host the child's title prefixed with the node's issue key (managed.childTitle), so the title is the document's
	r.release.Lock()
	defer r.release.Unlock()
	done, err := r.hostBound()
	if err != nil {
		return ops.ChildResult{}, err
	}
	defer done()
	res, err := r.Sched.Release(ctx, req.Plan, req.Node, r.Actor, request)
	if err != nil {
		if reason, held := holdBack(err); held && reason == ops.ReasonPlanChanged {
			// the plan gives the node another slice than the one the flow compared: no child is started, and the answer says why
			return ops.ChildResult{Reason: ops.ReasonPlanChanged}, nil
		} else if held {
			return ops.ChildResult{}, fmt.Errorf("refused:child_held_back: the scheduler holds node %s back (%s): no child is started: %v", req.Node, reason, err)
		}
		return ops.ChildResult{}, epochLost(req.Plan, err)
	}
	if !res.Bound {
		return ops.ChildResult{}, fmt.Errorf("refused:child_not_started: the managed start of node %s ended %s at %s (%s): no child is running; its intent and slot stay, and the same release continues it",
			req.Node, res.State, res.Stage, res.Reason)
	}
	return ops.ChildResult{Started: true, Child: res.ChildTaskID, Reason: req.Reason}, nil
}

// ---- acceptance and integration ----

// pointWorktree points the node's worktree handle at the worktree the journal says the node's work is in now (its newest fresh or
// restored allocation): a link replaced in one rename.
func (r *SchedRelay) pointWorktree(node string) error {
	wt := worktreeOf(r.Journal.Events(), node)
	if wt == "" {
		return nil
	}
	handle := r.worktreeHandle(node)
	if cur, err := os.Readlink(handle); err == nil && cur == wt {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(handle), 0o700); err != nil {
		return err
	}
	tmp := handle + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(wt, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, handle)
}

// worktreeOf is the newest worktree of a node in the journal: the path of its latest fresh or restored allocation, or of the worktree a
// resume suffix restored for it.
func worktreeOf(events []engine.Event, node string) string {
	path, seq := "", 0
	for _, ev := range events {
		switch {
		case ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "node") == node:
			if m, ok := ev.Data["allocation"].(map[string]any); ok && str(m, "path") != "" && (str(m, "kind") == engine.PlaceFresh || str(m, "kind") == engine.PlaceRestore) && ev.Seq > seq {
				path, seq = str(m, "path"), ev.Seq
			}
		case ev.Kind == engine.KindFlow && ev.Event == engine.EvWorktreeRestored && str(ev.Data, "node") == node && ev.Seq > seq:
			path, seq = str(ev.Data, "path"), ev.Seq
		case ev.Kind == engine.KindCall && ev.Op == ops.OpRelease && ev.Event == engine.EvCompleted && str(ev.Data, "node") == node && str(ev.Data, "kind") == "land" &&
			ev.Data["released"] == true && str(ev.Data, "worktree") != "" && ev.Seq > seq:
			// the worktree a land release checked a head made outside a run out in (CRW-1354)
			path, seq = str(ev.Data, "worktree"), ev.Seq
		}
	}
	return path
}

// agentReasons are the closed reasons of the agent path that travel in the detail of a disposition_conflict (dagsched/accept_agent.go).
var agentReasons = []string{dagsched.ReviewerMissing, dagsched.ReviewerNotVerified, dagsched.IndependenceRefused, dagsched.IndependenceUnproven, dagsched.AgentWorktreeUnrecorded, dagsched.AgentNotOrchestrator}

// refusalOf is the closed reason and the detail of a refusal: the agent path's own reason when the detail carries one.
func refusalOf(refused *store.RefusedError) (string, string) {
	for _, reason := range agentReasons {
		if strings.HasPrefix(refused.Detail, reason+":") {
			return reason, refused.Detail
		}
	}
	return refused.Reason, refused.Detail
}

// Accept is dag-accept on the agent path (CRW-1277 f): the commit the run made, over the base its verification stood on, in the plan's
// checkout, with the verification record and the pre-merge record the run assembled. A refusal is Accepted false with its reason; the new
// output of a node whose current acceptance stands on another head supersedes it.
func (r *SchedRelay) Accept(ctx context.Context, req ops.AcceptRequest) (ops.AcceptResult, error) {
	if err := r.claim(ctx, req.Plan); err != nil {
		return ops.AcceptResult{}, err
	}
	checkout, _, err := r.target(req.Plan)
	if err != nil {
		return ops.AcceptResult{}, err
	}
	if err := r.pointWorktree(req.Node); err != nil {
		return ops.AcceptResult{}, fmt.Errorf("the worktree of node %s cannot be named to the relay: %w", req.Node, err)
	}
	// the acceptance reads the calls of the execution from the store: the rows the run's journal still buffers are stored first
	if f, ok := r.Journal.(interface{ Flush() error }); ok {
		if err := f.Flush(); err != nil {
			return ops.AcceptResult{}, fmt.Errorf("the run's calls cannot be stored before the acceptance: %w", err)
		}
	}
	in := dagsched.AcceptInput{RuleVersion: r.Verifier, Premerge: req.Premerge,
		Commit: &dagsched.CommitRef{Head: req.Head, Base: req.Base, Checkout: checkout, Record: string(req.Verification)}}
	if cur, found, err := dagsched.CurrentAcceptance(ctx, r.q(ctx), req.Plan, req.Node); err != nil {
		return ops.AcceptResult{}, err
	} else if found && cur.Kind == dagsched.AcceptanceAgent && cur.Agent.Head != req.Head {
		in.Supersedes = cur.AcceptanceID
	}
	res, err := r.Sched.Accept(ctx, req.Plan, req.Node, r.Actor, in)
	if err != nil {
		var refused *store.RefusedError
		if errors.As(err, &refused) && refused.Reason != string(contract.RefusalStaleCoordinatorEpoch) {
			reason, detail := refusalOf(refused)
			return ops.AcceptResult{Reason: reason, Detail: detail}, nil
		}
		return ops.AcceptResult{}, epochLost(req.Plan, err)
	}
	return ops.AcceptResult{Accepted: true, AcceptanceID: res.AcceptanceID, Replayed: res.Replayed}, nil
}

// acceptedBy is the plan and node of an acceptance the run made, from its crw.accept rows.
func (r *SchedRelay) acceptedBy(id string) (plan, node string, ok bool) {
	for _, ev := range r.Journal.Events() {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpAccept && ev.Event == engine.EvCompleted && str(ev.Data, "acceptance_id") == id {
			plan, node, ok = str(ev.Data, "plan"), str(ev.Data, "node"), true
		}
	}
	return plan, node, ok
}

// AcceptState reads an acceptance the run made: active while it is the node's current agent acceptance, and stale once the plan's node
// no longer carries the criteria it was accepted under (or left the plan).
func (r *SchedRelay) AcceptState(ctx context.Context, id string) (ops.AcceptState, bool, error) {
	plan, node, ok := r.acceptedBy(id)
	if !ok {
		return ops.AcceptState{}, false, nil
	}
	q := r.q(ctx)
	cur, found, err := dagsched.CurrentAcceptance(ctx, q, plan, node)
	if err != nil {
		return ops.AcceptState{}, false, err
	}
	if !found || cur.Kind != dagsched.AcceptanceAgent || cur.AcceptanceID != id {
		return ops.AcceptState{}, true, nil
	}
	st := ops.AcceptState{Active: true, Head: cur.Agent.Head}
	snap, _, err := dag.SnapshotAt(ctx, q, plan, 0)
	if err != nil {
		return ops.AcceptState{}, false, err
	}
	st.Stale = true
	for _, n := range snap.Nodes {
		if n.NodeID == node && n.CriteriaSetDigest == cur.Agent.CriteriaSetDigest {
			st.Stale = false
		}
	}
	return st, true, nil
}

// Integrate lands an accepted head on the plan's target branch (CRW-1277 f): one integration batch of the node with the scheduler and the
// wiring of `crw relay dag-integrate` (dagsched.IntegrationDeps: the run's verification of the merged tree and git's compare-and-swap of
// the branch), then the observation of the landing on that branch, which marks the node integrated and returns its slot. A head the branch
// already holds is the replay of an integration that landed.
func (r *SchedRelay) Integrate(ctx context.Context, req ops.IntegrateRequest) (ops.IntegrateResult, error) {
	if err := r.claim(ctx, req.Plan); err != nil {
		return ops.IntegrateResult{}, err
	}
	checkout, ref, err := r.target(req.Plan)
	if err != nil {
		return ops.IntegrateResult{}, err
	}
	out := ops.IntegrateResult{IntegrationRef: ref}
	refused := func(err error) (ops.IntegrateResult, error) {
		var rf *store.RefusedError
		if errors.As(err, &rf) && rf.Reason != string(contract.RefusalStaleCoordinatorEpoch) {
			out.Reason, out.Detail = refusalOf(rf)
			return out, nil
		}
		return ops.IntegrateResult{}, epochLost(req.Plan, err)
	}
	q := r.q(ctx)
	cur, found, err := dagsched.CurrentAcceptance(ctx, q, req.Plan, req.Node)
	if err != nil {
		return ops.IntegrateResult{}, err
	}
	// the batch merges the node's current acceptance and nothing else, so the call has to name that acceptance and the head it accepted:
	// otherwise the answer would describe an integration other than the one the branch took
	if !found || cur.Kind != dagsched.AcceptanceAgent || cur.AcceptanceID != req.AcceptanceID {
		out.Reason, out.Detail = "acceptance_not_current", fmt.Sprintf("acceptance %q is not the current agent acceptance of node %s: nothing was integrated", req.AcceptanceID, req.Node)
		return out, nil
	}
	if req.Head == "" || cur.Agent.Head != req.Head {
		out.Reason, out.Detail = "head_not_accepted", fmt.Sprintf("acceptance %s accepted head %s, not %q: nothing was integrated", cur.AcceptanceID, cur.Agent.Head, req.Head)
		return out, nil
	}
	if landed, err := r.Sched.AgentAcceptanceIntegrated(ctx, q, cur.Agent); err != nil {
		return ops.IntegrateResult{}, err
	} else if landed {
		// the integration landed and was observed: the replay of a call whose answer the run did not record
		tip, err := r.IntegrationTip(ctx, req.Plan)
		if err != nil {
			return ops.IntegrateResult{}, err
		}
		out.Landed, out.Replayed, out.BatchCommit, out.Tip = true, true, tip, tip
		return out, nil
	}
	// the batch completes the marks an earlier batch left pending and never merges a head the branch already holds again
	res, err := r.Sched.IntegrateBatch(ctx, dagsched.IntegrationBatchInput{Plan: req.Plan, Actor: r.Actor, Checkout: checkout, IntegrationRef: ref, BaseRef: ref, Nodes: []string{req.Node}},
		dagsched.IntegrationDeps(r.Verify))
	if err != nil {
		var rf *store.RefusedError
		if !errors.As(err, &rf) || rf.Reason == string(contract.RefusalStaleCoordinatorEpoch) {
			return ops.IntegrateResult{}, epochLost(req.Plan, err)
		}
		// a node a batch merged before the run recorded it is no candidate any more: the branch holds its head, and only the observation
		// may be missing. Any other refusal stands
		tip, terr := r.IntegrationTip(ctx, req.Plan)
		if terr != nil {
			return ops.IntegrateResult{}, terr
		}
		if contained, cerr := isAncestor(ctx, checkout, req.Head, tip); cerr != nil || !contained {
			return refused(err)
		}
		out.Landed, out.Replayed, out.BatchCommit, out.Tip = true, true, tip, tip
	} else {
		for _, m := range res.Merged {
			if m.NodeID == req.Node {
				out.Landed, out.BatchCommit, out.Tip = true, res.NewHead, res.NewHead
			}
		}
		for _, c := range res.AlreadyContained {
			if c.NodeID == req.Node && !out.Landed {
				out.Landed, out.Replayed = true, true
				if out.Tip, err = r.IntegrationTip(ctx, req.Plan); err != nil {
					return ops.IntegrateResult{}, err
				}
				out.BatchCommit = out.Tip
			}
		}
		for _, sp := range res.Split {
			if sp.NodeID == req.Node && !out.Landed {
				out.Reason, out.Detail = sp.Reason, fmt.Sprintf("the batch %s left %s out: %s", res.BatchID, req.Node, sp.Reason)
			}
		}
		if !out.Landed {
			if out.Reason == "" {
				out.Reason, out.Detail = "not_integrated", fmt.Sprintf("the batch %s did not merge %s (merged %d, left out %d, contained unverified %d)", res.BatchID, req.Node, len(res.Merged), len(res.Split), len(res.ContainedUnverified))
			}
			return out, nil
		}
	}
	if r.testAfterBatch != nil {
		if err := r.testAfterBatch(); err != nil {
			return ops.IntegrateResult{}, err
		}
	}
	if _, err := r.Sched.ObserveIntegration(ctx, req.Plan, req.Node, r.Actor, []dagsched.Target{{Repository: checkout, BaseRef: ref}}); err != nil {
		return refused(err)
	}
	return out, nil
}

// IntegrationTip is the commit the plan's target branch holds now.
func (r *SchedRelay) IntegrationTip(ctx context.Context, plan string) (string, error) {
	checkout, ref, err := r.target(plan)
	if err != nil {
		return "", err
	}
	tip, err := r.Sched.Tips.Tip(ctx, checkout, ref)
	if err != nil {
		return "", err
	}
	return tip.SHA, nil
}

// Close is AgentClose (CRW-1277 f): the node's current execution is closed (a node given up or cancelled returns its slot in the same
// transaction) or held. A node the relay never gave an execution has nothing to close.
func (r *SchedRelay) Close(ctx context.Context, req ops.CloseRequest) (ops.CloseResult, error) {
	if req.Plan == "" {
		return ops.CloseResult{}, nil
	}
	executed, err := dagsched.ExecutedNodes(ctx, r.q(ctx), req.Plan)
	if err != nil {
		return ops.CloseResult{}, err
	}
	if !executed[req.Node] {
		return ops.CloseResult{}, nil
	}
	if err := r.claim(ctx, req.Plan); err != nil {
		return ops.CloseResult{}, err
	}
	res, err := r.Sched.AgentClose(ctx, req.Plan, req.Node, r.Actor, dagsched.AgentCloseInput{Reason: req.Reason})
	if err != nil {
		// the relay keeps the execution (its accepted head landed or may be in the target, or there is none open to close): the
		// lane's kind of it, which a gate-only land records and does not ask again (CRW-1397)
		var refused *store.RefusedError
		if errors.As(err, &refused) && refused.Reason == string(contract.RefusalDispositionConflict) {
			return ops.CloseResult{}, fmt.Errorf("%w: %w", ops.ErrCloseConflict, err)
		}
		return ops.CloseResult{}, epochLost(req.Plan, err)
	}
	return ops.CloseResult{Closed: res.Closed, Held: res.Held, SlotReleased: res.SlotReleased, Replayed: res.Replayed, ExecutionID: res.ExecutionID, State: res.State}, nil
}

// isAncestor says whether commit is an ancestor of (or is) tip in the checkout.
func isAncestor(ctx context.Context, checkout, commit, tip string) (bool, error) {
	if commit == "" || tip == "" {
		return false, nil
	}
	return ops.IsAncestor(ctx, ops.ExecGit("git"), checkout, commit, tip)
}
