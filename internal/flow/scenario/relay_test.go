package scenario

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// NodeSpec is a node of the lab's plan.
type NodeSpec struct {
	ID, Issue, Title string
	After, Regions   []string
	Group            string
	// SizeFit is the architect's size_fit for the node's issue (executor when empty).
	SizeFit, SizeReason string
	Criteria            []string
	// EndCondition are the commands of the issue's done condition, as the relay reads them from the body the handoff carries (CRW-1382).
	EndCondition []string
	// Planned are the node's regions with their planned change and hold, as the relay passes them when one is not a plain edit
	// (CRW-1461): the whole list, in the order of Regions.
	Planned []ops.LaneRegion
}

func (n NodeSpec) criteria() []string {
	if len(n.Criteria) > 0 {
		return n.Criteria
	}
	return []string{"criterion of " + n.ID}
}

func sha(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// FakeRelay is the relay's DAG scheduler as the lane sees it, over the lab's repository: the plan's nodes, the slot of a release,
// an acceptance that judges the premerge record the run assembled, an integration that merges the head into the integration
// branch of the repository, and the closure of an execution that returns its slot. It records every request, so a scenario can
// say what the lane asked of the scheduler.
type FakeRelay struct {
	lab *Lab

	mu sync.Mutex
	// Slots is the number of slots; a release over it is refused capacity_exhausted (0: unlimited).
	Slots int
	// RefuseAccept makes the relay refuse an acceptance with this reason.
	RefuseAccept string
	// ReleaseErr, when it answers an error for a node, makes the release of that node fail with it (a refusal the release op raises, as
	// the scheduler's stale coordinator epoch is); HoldBack holds every release back unreleased with this reason (CRW-1320).
	ReleaseErr func(node string) error
	HoldBack   string
	// HoldBackIf, when it answers a reason for a release, holds that release back unreleased with it (the relay's own judgment of the
	// release, as relayRegionRule makes it): it is asked after HoldBack and the replay of a release, with the regions of the nodes the
	// relay holds released (holding: the request's regions of each, carrying the planned change and hold of the node's Planned by the
	// normal form, as declareRegions declares them; a node stops holding them at its integration or its close).
	HoldBackIf func(req ops.ReleaseRequest, holding map[string][]ops.LaneRegion) string
	// OnRelease runs at the top of every release and OnIntegrate at the top of every integration, outside the relay's lock, so a scenario
	// may block on a channel there to order what the lanes do.
	OnRelease   func(req ops.ReleaseRequest)
	OnIntegrate func(node string)
	// Surfaces, Grade and Commands are what the plan says of the milestone: the surfaces of its criteria, the work grade and the
	// required verification commands (CRW-1206).
	Surfaces []string
	Grade    string
	Commands []string

	nodes    []NodeSpec
	released map[string]ops.ReleaseResult
	held     map[string]bool
	n        int
	clock    int
	accepted map[string]ops.AcceptRequest
	holding  map[string][]ops.LaneRegion

	Releases   []ops.ReleaseRequest
	Accepts    []ops.AcceptRequest
	Integrates []ops.IntegrateRequest
	Closes     []ops.CloseRequest
	// Landed are the nodes in the order they landed.
	Landed []string
}

func newRelay(l *Lab) *FakeRelay {
	return &FakeRelay{lab: l, released: map[string]ops.ReleaseResult{}, held: map[string]bool{}, accepted: map[string]ops.AcceptRequest{}}
}

// ChangeNode changes a node of the plan the relay reads: the plan revision an orchestrator makes with update_node. The slice digest
// of the node changes with its regions, its dependencies or its issue.
func (r *FakeRelay) ChangeNode(id string, change func(*NodeSpec)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	nodes := append([]NodeSpec(nil), r.nodes...)
	for i := range nodes {
		if nodes[i].ID == id {
			change(&nodes[i])
		}
	}
	r.nodes = nodes
}

// Milestone implements ops.Relay.
func (r *FakeRelay) Milestone(_ context.Context, plan, milestone string) (ops.Milestone, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ms := ops.Milestone{ID: milestone, Plan: plan, Title: "milestone " + milestone, Criteria: []string{"the milestone is done"}, CriteriaDigest: sha("milestone " + milestone),
		Surfaces: r.Surfaces, Grade: r.Grade, RequiredCommands: r.Commands}
	for _, n := range r.nodes {
		pn := ops.PlanNode{ID: n.ID, Issue: n.Issue, Title: n.Title, After: n.After, Regions: n.Regions, Group: n.Group, Criteria: n.criteria(), EndCondition: n.EndCondition,
			PlannedRegions: n.Planned}
		pn.CriteriaDigest = sha(strings.Join(pn.Criteria, "\n"))
		pn.SliceDigest = sliceOf(n)
		ms.Nodes = append(ms.Nodes, pn)
	}
	return ms, nil
}

// sliceOf is a node's slice of the plan; the planned attributes are part of it only when the node has them, so a node without them
// keeps the slice it had.
func sliceOf(n NodeSpec) string {
	if len(n.Planned) > 0 {
		return sha(fmt.Sprintf("%s|%s|%v|%v|%v", n.ID, n.Issue, n.After, n.Regions, n.Planned))
	}
	return sha(fmt.Sprintf("%s|%s|%v|%v", n.ID, n.Issue, n.After, n.Regions))
}

// Release implements ops.Relay. Like the relay, it compares the slice the flow compared with the node's slice of the plan it holds
// when it records the release (CRW-1288), answers a replay only under the slice it was released under, and answers that slice.
func (r *FakeRelay) Release(_ context.Context, req ops.ReleaseRequest) (ops.ReleaseResult, error) {
	if r.OnRelease != nil {
		r.OnRelease(req)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Releases = append(r.Releases, req)
	if r.ReleaseErr != nil {
		if err := r.ReleaseErr(req.Node); err != nil {
			return ops.ReleaseResult{}, err
		}
	}
	if r.HoldBack != "" {
		return ops.ReleaseResult{Reason: r.HoldBack}, nil
	}
	if prior, ok := r.released[req.Node]; ok && r.held[req.Node] {
		if req.SliceDigest != "" && req.SliceDigest != prior.SliceDigest {
			return ops.ReleaseResult{Reason: ops.ReasonPlanChanged}, nil
		}
		prior.Replayed = true
		return prior, nil
	}
	if r.HoldBackIf != nil {
		holding := make(map[string][]ops.LaneRegion, len(r.holding))
		for node, regions := range r.holding {
			if node != req.Node {
				holding[node] = regions
			}
		}
		if reason := r.HoldBackIf(req, holding); reason != "" {
			return ops.ReleaseResult{Reason: reason}, nil
		}
	}
	slice := ""
	for _, n := range r.nodes {
		if n.ID == req.Node {
			slice = sliceOf(n)
		}
	}
	if req.SliceDigest != "" && req.SliceDigest != slice {
		return ops.ReleaseResult{Reason: ops.ReasonPlanChanged}, nil
	}
	used := 0
	for _, h := range r.held {
		if h {
			used++
		}
	}
	if r.Slots > 0 && used >= r.Slots {
		return ops.ReleaseResult{Reason: "capacity_exhausted"}, nil
	}
	r.n++
	res := ops.ReleaseResult{Released: true, ExecutionID: fmt.Sprintf("exec-%s-%d", req.Node, r.n), SlotID: "slot-" + req.Node, SliceDigest: slice}
	r.released[req.Node] = res
	r.held[req.Node] = true
	if r.holding == nil {
		r.holding = map[string][]ops.LaneRegion{}
	}
	r.holding[req.Node] = r.plannedRegions(req.Node, req.Regions)
	return res, nil
}

// plannedRegions are a node's request regions as the relay declares them: a region whose normal form is one of the node's Planned
// regions carries that region's change and hold.
func (r *FakeRelay) plannedRegions(node string, texts []string) []ops.LaneRegion {
	var planned []ops.LaneRegion
	for _, n := range r.nodes {
		if n.ID == node {
			planned = n.Planned
		}
	}
	out := make([]ops.LaneRegion, 0, len(texts))
	for _, t := range texts {
		reg := ops.LaneRegion{Text: t}
		for _, p := range planned {
			if ops.RegionOf(p.Text) == ops.RegionOf(t) {
				reg.Change, reg.Exclusive = p.Change, p.Exclusive
				break
			}
		}
		out = append(out, reg)
	}
	return out
}

// relayRegionRule is the relay's region judgment of a release (dagsched.Overlaps over the declared normal forms, with the planned change
// and hold): a release whose regions overlap a region another released node holds is held back defer:edit_overlap. A node with no
// regions, on either side, overlaps every other.
func (r *FakeRelay) relayRegionRule(req ops.ReleaseRequest, holding map[string][]ops.LaneRegion) string {
	mine := r.plannedRegions(req.Node, req.Regions)
	for _, theirs := range holding {
		if len(mine) == 0 || len(theirs) == 0 {
			return dagsched.DeferEditOverlap
		}
		for _, a := range mine {
			for _, b := range theirs {
				if dagsched.Overlaps(relayRegion(a), relayRegion(b)) {
					return dagsched.DeferEditOverlap
				}
			}
		}
	}
	return ""
}

// relayRegion is a region in the relay's form: its normal form, with its change and hold.
func relayRegion(r ops.LaneRegion) dagsched.Region {
	f := ops.RegionOf(r.Text)
	return dagsched.Region{Path: f.Path, Kind: f.Kind, Key: f.Key, Change: r.Change, Exclusive: r.Exclusive}
}

// ReleaseStands implements ops.Relay.
func (r *FakeRelay) ReleaseStands(_ context.Context, _, node, execution string) (bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.released[node].ExecutionID == execution && execution != "", nil
}

// Accept implements ops.Relay: the relay judges the premerge record as the real acceptance does.
func (r *FakeRelay) Accept(ctx context.Context, req ops.AcceptRequest) (ops.AcceptResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Accepts = append(r.Accepts, req)
	if r.RefuseAccept != "" {
		return ops.AcceptResult{Reason: r.RefuseAccept, Detail: "refused by the scenario"}, nil
	}
	// The relay judges what the acceptance of R05 judges (relay/dagsched/accept_agent.go): the premerge record decodes, stands
	// on the head and the base the acceptance names, and passes the judgment; the verification record is a pass of this head
	// over this base; the base is an ancestor of the head; the record sees every criterion of the node's plan.
	rec, err := premerge.Decode(req.Premerge)
	if err != nil {
		return ops.AcceptResult{Reason: "premerge_missing", Detail: err.Error()}, nil
	}
	if err := premerge.Judge(rec, premerge.Options{}); err != nil {
		return ops.AcceptResult{Reason: "premerge_blocked", Detail: err.Error()}, nil
	}
	if rec.Head != req.Head || rec.Dev != req.Base || rec.CriteriaDigest != req.CriteriaDigest {
		return ops.AcceptResult{Reason: "premerge_head_mismatch", Detail: fmt.Sprintf("the premerge record names head %s dev %s criteria %s, the acceptance %s %s %s", rec.Head, rec.Dev, rec.CriteriaDigest, req.Head, req.Base, req.CriteriaDigest)}, nil
	}
	if len(req.Verification) == 0 {
		return ops.AcceptResult{Reason: "verification_missing"}, nil
	}
	var ver struct {
		Schema, HeadCommit, Base, Result string
	}
	if err := json.Unmarshal(req.Verification, &ver); err != nil || ver.Schema != "verification-record/1" {
		return ops.AcceptResult{Reason: "verification_missing", Detail: "the verification is no verification-record/1"}, nil
	}
	if ver.Result != "pass" || ver.HeadCommit != req.Head || ver.Base != req.Base {
		return ops.AcceptResult{Reason: "verification_stale", Detail: fmt.Sprintf("the verification record is %s of head %s over base %s; the acceptance names head %s over base %s", ver.Result, ver.HeadCommit, ver.Base, req.Head, req.Base)}, nil
	}
	if out, err := r.lab.gitErr(r.lab.Repo, "cat-file", "-e", req.Head+"^{commit}"); err != nil {
		return ops.AcceptResult{Reason: "head_not_receipt_head", Detail: out}, nil
	}
	if out, err := r.lab.gitErr(r.lab.Repo, "merge-base", "--is-ancestor", req.Base, req.Head); err != nil {
		return ops.AcceptResult{Reason: "merge_base_mismatch", Detail: "the base " + req.Base + " is not an ancestor of the head " + req.Head + ": " + out}, nil
	}
	for _, n := range r.nodes {
		if n.ID != req.Node {
			continue
		}
		want := map[string]bool{}
		for i := range n.criteria() {
			want[fmt.Sprintf("c%d", i+1)] = true
		}
		for id := range rec.Criteria {
			delete(want, id)
		}
		if len(want) > 0 {
			return ops.AcceptResult{Reason: "criteria_set_changed", Detail: fmt.Sprintf("the premerge record leaves %d criteria of the plan unevaluated", len(want))}, nil
		}
	}
	id := "acc-" + req.Node + "-" + req.Head[:8]
	_, replayed := r.accepted[id]
	r.accepted[id] = req
	return ops.AcceptResult{Accepted: true, AcceptanceID: id, Replayed: replayed}, nil
}

// AcceptState implements ops.Relay.
func (r *FakeRelay) AcceptState(_ context.Context, id string) (ops.AcceptState, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	req, ok := r.accepted[id]
	return ops.AcceptState{Active: true, Head: req.Head}, ok, nil
}

// Integrate implements ops.Relay: the head is merged into the integration branch (a fast-forward when the branch has not moved
// past the head's base, else a merge commit), and the slot of the node returns.
func (r *FakeRelay) Integrate(_ context.Context, req ops.IntegrateRequest) (ops.IntegrateResult, error) {
	if r.OnIntegrate != nil {
		r.OnIntegrate(req.Node)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Integrates = append(r.Integrates, req)
	if _, ok := r.accepted[req.AcceptanceID]; !ok {
		return ops.IntegrateResult{Reason: "disposition_conflict", Detail: "no such acceptance"}, nil
	}
	l := r.lab
	tip := l.git(l.Repo, "rev-parse", IntegrationRef)
	if _, err := l.gitErr(l.Repo, "merge-base", "--is-ancestor", req.Head, tip); err == nil {
		// a head already on the tip lands as a replay; the node is landed all the same, so it holds neither its slot nor its regions
		// (a replay after a real land finds them clear)
		r.held[req.Node] = false
		delete(r.holding, req.Node)
		return ops.IntegrateResult{Landed: true, BatchCommit: tip, IntegrationRef: IntegrationRef, Tip: tip, Replayed: true}, nil
	}
	batch := req.Head
	if _, err := l.gitErr(l.Repo, "merge-base", "--is-ancestor", tip, req.Head); err != nil {
		tree, err := l.gitErr(l.Repo, "merge-tree", "--write-tree", tip, req.Head)
		if err != nil {
			return ops.IntegrateResult{Reason: "disposition_conflict", Detail: "the head does not merge into the integration branch: " + tree}, nil
		}
		// every merge commit has a time of its own (a clock that counts), so a merge made again is another commit
		r.clock++
		stamp := fmt.Sprintf("@%d +0000", 1790000000+r.clock)
		cmd := exec.Command("git", "-C", l.Repo, "commit-tree", strings.TrimSpace(strings.Split(tree, "\n")[0]), "-p", tip, "-p", req.Head, "-m", "integrate "+req.Node)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
		out, err := cmd.CombinedOutput()
		if err != nil {
			return ops.IntegrateResult{}, fmt.Errorf("commit-tree: %v: %s", err, out)
		}
		batch = strings.TrimSpace(string(out))
	}
	l.git(l.Repo, "update-ref", IntegrationRef, batch)
	r.Landed = append(r.Landed, req.Node)
	r.held[req.Node] = false
	delete(r.holding, req.Node)
	return ops.IntegrateResult{Landed: true, BatchCommit: batch, IntegrationRef: IntegrationRef, Tip: batch}, nil
}

// IntegrationTip implements ops.Relay.
func (r *FakeRelay) IntegrationTip(_ context.Context, _ string) (string, error) {
	return r.lab.git(r.lab.Repo, "rev-parse", IntegrationRef), nil
}

// Close implements ops.Relay: AgentClose. A node given up returns its slot; a recovery hold closes nothing.
func (r *FakeRelay) Close(_ context.Context, req ops.CloseRequest) (ops.CloseResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Closes = append(r.Closes, req)
	switch req.Reason {
	case "dispatch_unknown", "host_failure":
		return ops.CloseResult{Held: true, ExecutionID: r.released[req.Node].ExecutionID}, nil
	}
	was := r.held[req.Node]
	exec := r.released[req.Node].ExecutionID
	r.held[req.Node] = false
	delete(r.holding, req.Node)
	// as the interface says, a closed execution does not stand: ReleaseStands is false for it from now on
	delete(r.released, req.Node)
	return ops.CloseResult{Closed: true, SlotReleased: was, State: "escalated", ExecutionID: exec}, nil
}

// SlotHeld says whether the node holds a slot.
func (r *FakeRelay) SlotHeld(node string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held[node]
}

// CloseReasons are the reasons of the closes asked of the relay, in order.
func (r *FakeRelay) CloseReasons() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, c := range r.Closes {
		out = append(out, c.Node+":"+c.Reason)
	}
	return out
}

// IntegrationRef is the branch the lab's relay integrates into.
const IntegrationRef = "refs/heads/integration"

// FakeVerifier answers the verification of a head: a pass unless the scenario says the nth verification of a node fails.
type FakeVerifier struct {
	mu sync.Mutex
	// Fail lists, by node, the verifications (counted from 1) that fail.
	Fail map[string][]int
	// Unavailable makes the verification of a node not run at all.
	Unavailable map[string]bool
	n           map[string]int
	Calls       []ops.VerifyRequest
}

// Verify implements ops.Verifier.
func (v *FakeVerifier) Verify(_ context.Context, req ops.VerifyRequest) (ops.VerifyOutcome, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.n == nil {
		v.n = map[string]int{}
	}
	v.Calls = append(v.Calls, req)
	if v.Unavailable[req.Node] {
		return ops.VerifyOutcome{}, fmt.Errorf("crw-dev is not installed")
	}
	v.n[req.Node]++
	pass := true
	for _, k := range v.Fail[req.Node] {
		if k == v.n[req.Node] {
			pass = false
		}
	}
	rec := map[string]any{"schema": "verification-record/1", "headCommit": req.Head, "base": req.Base, "result": map[bool]string{true: "pass", false: "fail"}[pass]}
	raw, _ := json.Marshal(rec)
	out := ops.VerifyOutcome{Pass: pass, Command: ops.CILocalCommand, Digest: "sha256:" + sha(string(raw)), Record: raw, Summary: "pass", Failures: []string{}}
	if !pass {
		out.Summary = "validate/tests: failed"
		out.Failures = []string{"validate/tests: failed"}
	}
	return out, nil
}

// compile-time checks
var (
	_ ops.Relay    = (*FakeRelay)(nil)
	_ ops.Verifier = (*FakeVerifier)(nil)
)

func (l *Lab) git(dir string, args ...string) string {
	l.t.Helper()
	out, err := l.gitErr(dir, args...)
	if err != nil {
		l.t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return out
}

func (l *Lab) gitErr(dir string, args ...string) (string, error) {
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	return strings.TrimSpace(string(out)), err
}

// AdvanceIntegration moves the integration branch on by a commit of another actor that adds a file: a landing that happens while
// a node of the run is being verified or reviewed. It returns the new tip.
func (l *Lab) AdvanceIntegration(file string) string {
	l.t.Helper()
	l.advanceMu.Lock()
	defer l.advanceMu.Unlock()
	tip := l.git(l.Repo, "rev-parse", IntegrationRef)
	idx := filepath.Join(l.Root, "advance.index")
	run := func(stdin string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-C", l.Repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_INDEX_FILE="+idx)
		cmd.Stdin = strings.NewReader(stdin)
		out, err := cmd.CombinedOutput()
		if err != nil {
			l.t.Errorf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	_ = os.Remove(idx)
	run("", "read-tree", tip)
	blob := run("outside landing of "+file+"\n", "hash-object", "-w", "--stdin")
	run("", "update-index", "--add", "--cacheinfo", "100644,"+blob+","+file)
	tree := run("", "write-tree")
	commit := run("", "commit-tree", tree, "-p", tip, "-m", "landing of "+file)
	l.git(l.Repo, "update-ref", IntegrationRef, commit)
	return commit
}
