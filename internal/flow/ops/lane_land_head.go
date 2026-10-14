package ops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The release of a head made outside a project-lane run (CRW-1354). The gate-only workflow land-head lands a head the child made with
// its own subagents, out of any run, through the gates a head of a run goes through: crw.verify, crw.review, crw.accept and crw.integrate,
// which write the rows they write for any lane. What those gates read of the node's work in a run - the release of the node to the run,
// the worktree the head is in, the pairs that wrote it - is what crw.release with opts.land gives them, and nothing more:
//
//   - the release is the relay's agent release of the node (Relay.Release, dag-release --surface agent), pinned to the plan the run read
//     (the crw.milestone row) and carrying the node's slice, as every release; its row is the crw.release row the gates read;
//   - the head is resolved in the run's repository and checked out, detached, in a worktree of the run's own (LandRoot), which the row
//     names and the execution's worktree handle points at, so the relay reads the head from git there;
//   - the authors are the threads the child names, each read back from the host (thread/read: the configured pair). No executor call is
//     invented: the observed pairs are the row's, crw.review judges the reviewer's family against them, and the release records the pair
//     on the node's execution row (author_model, author_effort), which the relay's acceptance judges against.
//
// The refusals are input errors only (no head, a head git cannot resolve, authors of more than one model family, whose pairs one
// execution row cannot hold): released false with the reason, nothing written at the relay. A named thread whose pair cannot be read back,
// no named thread, and paths outside the node's regions are warnings of the row; with any named thread not read back, or none named, the
// independence gate refuses the review (independence_unproven), and nothing else stops the land.

// LandOpts are the head and the authors a land release names.
type LandOpts struct {
	Head    string   `json:"head"`
	Authors []string `json:"authors"`
}

// LandAuthor is an author thread the run read back: the configured pair, or why it was not observed.
type LandAuthor struct {
	Thread   string `json:"thread"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
	Observed bool   `json:"observed"`
	Reason   string `json:"reason,omitempty"`
}

// LandHead is the head a land release checked out for the gates: the commit as crw.verify, crw.review and crw.accept take it (commit,
// tree, the paths it changes over its base and the ones outside the node's regions), the authors and the warnings.
type LandHead struct {
	Commit     string       `json:"commit"`
	Tree       string       `json:"tree"`
	Base       string       `json:"base"`
	Changed    []string     `json:"changed"`
	OutOfScope []string     `json:"out_of_scope"`
	Worktree   string       `json:"worktree"`
	Authors    []LandAuthor `json:"authors"`
	Warnings   []string     `json:"warnings"`
}

// The reasons of a land release that released nothing because of its input, and its warnings.
const (
	ReasonLandHeadMissing      = "land_head_missing"
	ReasonLandHeadUnresolved   = "land_head_unresolved"
	ReasonLandAuthorsMixed     = "land_author_families_mixed"
	ReasonLandNodeHeld         = "land_node_held"
	WarnLandAuthorsUnnamed     = "land_authors_unnamed"
	WarnLandAuthorUnobserved   = "land_author_unobserved"
	WarnLandOutOfScope         = "land_out_of_scope"
	WarnLandHeldByEndedRun     = "land_node_held_by_ended_run"
	WarnLandPairUnrecorded     = "land_author_pair_unrecorded"
	landAuthorNoReader         = "no_host_reader"
	landAuthorReadFailed       = "read_failed"
	landAuthorNullModel        = "null_model"
	landWorktreeDirectoryName  = "land-wt"
	landWorktreeCommitIDLength = 12
)

// ThreadConfigs reads the configured pair of a thread on the host (thread/read: Thread.model and Thread.reasoningEffort, nil for a null).
// The run's Threads seam provides it when the host can be asked.
type ThreadConfigs interface {
	ThreadConfig(ctx context.Context, threadID string) (model, effort *string, err error)
}

// readAuthors reads back the pair of each author thread.
func (l *Lane) readAuthors(ctx context.Context, threads []string) ([]LandAuthor, []string) {
	reader, _ := l.d.Threads.(ThreadConfigs)
	out := make([]LandAuthor, 0, len(threads))
	var warnings []string
	if len(threads) == 0 {
		warnings = append(warnings, WarnLandAuthorsUnnamed+": the land names no author thread, so no author pair is observed and the review cannot be shown independent")
	}
	for _, id := range threads {
		a := LandAuthor{Thread: id}
		switch {
		case reader == nil:
			a.Reason = landAuthorNoReader
		default:
			model, effort, err := reader.ThreadConfig(ctx, id)
			switch {
			case err != nil:
				a.Reason = landAuthorReadFailed
			case model == nil || strings.TrimSpace(*model) == "":
				a.Reason = landAuthorNullModel
			default:
				// the pair as the relay judges it: the host's spelling trimmed, so blanks around it are not a malformed receipt (CRW-1397)
				a.Model, a.Observed = strings.TrimSpace(*model), true
				if effort != nil {
					a.Effort = strings.TrimSpace(*effort)
				}
			}
		}
		if !a.Observed {
			warnings = append(warnings, fmt.Sprintf("%s: the pair of author thread %s was not read back (%s): it is no observed author", WarnLandAuthorUnobserved, id, a.Reason))
		}
		out = append(out, a)
	}
	return out, warnings
}

// runOver says whether a run state names a run that has ended (the states of cli.Over: completed, blocked, cancelled, failed).
func runOver(state string) bool {
	switch state {
	case "completed", "blocked", "cancelled", "failed":
		return true
	}
	return false
}

// landRelease is crw.release(plan, node, milestone, {pinned, land}): see the comment at the top of the file.
func (o *releaseOp) landRelease(ctx context.Context, req engine.OpRequest, plan, node, ms string, opts ReleaseOpts) (engine.OpResult, error) {
	r := rows(req.Journal.Events())
	out := ReleaseOutcome{Node: node, Kind: "land"}
	data := map[string]any{"plan": plan, "node": node, "milestone": ms}
	finish := func() (engine.OpResult, error) {
		data["released"], data["reason"], data["kind"] = out.Released, out.Reason, out.Kind
		if out.Detail != "" {
			data["detail"] = out.Detail
		}
		return engine.OpResult{Result: result(out), Data: data}, nil
	}
	refusedInput := func(reason, detail string) (engine.OpResult, error) {
		out.Reason, out.Detail = reason, detail
		return finish()
	}
	land := opts.Land
	if strings.TrimSpace(land.Head) == "" {
		return refusedInput(ReasonLandHeadMissing, "the land names no head: give the commit or the branch the child made")
	}
	g := o.l.d.Git
	commit, err := g(ctx, o.l.d.Repo, nil, "rev-parse", "--verify", "--end-of-options", land.Head+"^{commit}")
	if err != nil || commit == "" {
		return refusedInput(ReasonLandHeadUnresolved, fmt.Sprintf("git resolves no commit %q in %s", land.Head, o.l.d.Repo))
	}
	authors, warnings := o.l.readAuthors(ctx, land.Authors)
	families := map[string]bool{}
	var pair *LandAuthor
	for i := range authors {
		if a := authors[i]; a.Observed {
			families[execution.ModelFamily(a.Model)] = true
			if pair == nil {
				pair = &authors[i]
			}
		}
	}
	data["head"], data["head_named"], data["authors"] = commit, land.Head, authors
	if len(families) > 1 {
		return refusedInput(ReasonLandAuthorsMixed, fmt.Sprintf("the author threads ran on %d model families: the node's execution records one author pair, so a head of several families is landed as one land per family's work", len(families)))
	}
	// the plan the run read (its crw.milestone row) is the one the land stands on, compared with the relay's now as every release does
	m, err := o.l.liveMilestone(ctx, plan, ms)
	if err != nil {
		return engine.OpResult{}, err
	}
	pinned := ""
	if row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "plan") == plan && dataStr(ev, "milestone") == ms }); ok {
		pinned = dataStr(row, "digest")
	}
	live := digestOf(m)
	data["plan_digest"], data["pinned_plan"] = live, pinned
	if opts.Pinned && live != pinned {
		out.Reason = ReasonPlanChanged
		return finish()
	}
	slice, inPlan := liveSlices(m)[node]
	if !inPlan {
		out.Reason = "node_not_in_plan"
		return finish()
	}
	var regions []string
	criteria := ""
	for _, n := range m.Nodes {
		if n.ID == node {
			regions, criteria = n.Regions, n.CriteriaDigest
		}
	}
	data["slice_digest"], data["criteria_digest"] = slice, criteria
	rq := ReleaseRequest{Plan: plan, Node: node, RunID: req.RunID, RequestID: req.RequestID, SliceDigest: slice, PlanDigest: live, Land: true}
	unproven := false
	for _, a := range authors {
		unproven = unproven || !a.Observed
	}
	switch {
	case pair != nil && unproven:
		// an author set one of whose threads was not read back is unproven (landAuthorModels): the observed pair would stand on the
		// execution for authors the run does not know, so the execution records none (CRW-1397), warned and recorded
		warnings = append(warnings, fmt.Sprintf("%s: an author of the land was not read back, so the node's execution records no author pair (the observed %s %s is on this row)",
			WarnLandPairUnrecorded, pair.Model, pair.Effort))
	case pair != nil:
		rq.AuthorModel, rq.AuthorEffort = pair.Model, pair.Effort
	}
	res, err := o.l.d.Relay.Release(ctx, rq)
	if errors.Is(err, ErrEpochLost) {
		out.Reason, out.Detail = ReasonEpochLost, err.Error()
		return finish()
	}
	if err != nil {
		return engine.OpResult{}, err
	}
	held := func(run, execution, state string) (engine.OpResult, error) {
		// the node's current execution is another run's (a run that has not ended, or one killed after its release): a head of it is
		// accepted only on a review of that run, so this land releases nothing and runs no gate
		out.Released, out.Reason, out.ExecutionID = false, ReasonLandNodeHeld, ""
		out.Detail = fmt.Sprintf("node %s is held by run %s (execution %s), which has not given it up: crw flow resume %s goes on with what it holds, and a land of the "+
			"node waits until that run has ended", node, run, execution, run)
		if runOver(state) {
			// a run that has ended is not resumed (CRW-1397): the command that gives its execution up is its cancel; warned and recorded,
			// the land refuses nothing more than it did
			out.Detail = fmt.Sprintf("node %s is held by run %s (execution %s), which has ended (%s) without giving it up: crw flow cancel %s gives that execution up "+
				"and returns its slot, and the node is then landed again", node, run, execution, state, run)
			data["warnings"] = []string{WarnLandHeldByEndedRun + ": " + out.Detail}
		}
		data["held_run"], data["held_execution"], data["execution_id"] = run, execution, ""
		if state != "" {
			data["held_run_state"] = state
		}
		return finish()
	}
	if res.Reason == ReasonLandNodeHeld {
		return held(res.HeldRun, res.HeldExecution, res.HeldRunState)
	}
	out.Released, out.Reason, out.ExecutionID = res.Released, res.Reason, res.ExecutionID
	data["execution_id"], data["slot_id"], data["request_id"] = res.ExecutionID, res.SlotID, req.RequestID
	if !res.Released {
		return finish()
	}
	if res.Run != "" && res.Run != req.RunID {
		// the replay of the execution another run holds carries that run's state as the first answer does (CRW-1404)
		return held(res.Run, res.ExecutionID, res.HeldRunState)
	}
	data["slice_digest"] = res.SliceDigest
	wt, err := o.l.landWorktree(ctx, node, commit)
	if err != nil {
		return engine.OpResult{}, err
	}
	head := LandHead{Commit: commit, Worktree: wt, Authors: authors, Changed: []string{}, OutOfScope: []string{}}
	if head.Tree, err = TreeOf(ctx, g, wt, commit); err != nil {
		return engine.OpResult{}, err
	}
	if head.Base, err = o.l.stableBase(ctx, plan, wt, commit); err != nil {
		return engine.OpResult{}, err
	}
	scope, err := Scope(ctx, g, wt, head.Base, head.Tree, regions)
	if err != nil {
		return engine.OpResult{}, err
	}
	head.Changed, head.OutOfScope = scope.Changed, scope.OutOfScope
	if len(scope.OutOfScope) > 0 {
		warnings = append(warnings, fmt.Sprintf("%s: the head changes %s outside the node's regions", WarnLandOutOfScope, strings.Join(scope.OutOfScope, ", ")))
	}
	head.Warnings = warnings
	if head.Warnings == nil {
		head.Warnings = []string{}
	}
	out.Land = &head
	data["worktree"], data["tree"], data["base"], data["out_of_scope"], data["warnings"] = wt, head.Tree, head.Base, head.OutOfScope, head.Warnings
	return finish()
}

// landWorktree is the run's worktree of a landed head: a detached checkout of the commit under LandRoot, made once; a resume finds it at
// the commit and uses it again.
func (l *Lane) landWorktree(ctx context.Context, node, commit string) (string, error) {
	if l.d.LandRoot == "" {
		return "", fmt.Errorf("crw.release: the land of %s has no directory for its worktree (Deps.LandRoot)", node)
	}
	name := strings.Map(func(c rune) rune {
		if c == '/' || c == '\\' || c == 0 {
			return '_'
		}
		return c
	}, node)
	id := commit
	if len(id) > landWorktreeCommitIDLength {
		id = id[:landWorktreeCommitIDLength]
	}
	wt := filepath.Join(l.d.LandRoot, landWorktreeDirectoryName, name+"-"+id)
	if _, err := os.Stat(wt); err == nil {
		head, err := Head(ctx, l.d.Git, wt)
		if err != nil || head != commit {
			return "", fmt.Errorf("crw.release: the land worktree %s of %s is not at %s (%q, %v)", wt, node, commit, head, err)
		}
		return wt, nil
	}
	if err := os.MkdirAll(filepath.Dir(wt), 0o700); err != nil {
		return "", err
	}
	if _, err := l.d.Git(ctx, l.d.Repo, nil, "worktree", "add", "--detach", wt, commit); err != nil {
		return "", fmt.Errorf("crw.release: the land worktree of %s at %s: %w", node, commit, err)
	}
	return wt, nil
}

// landAuthorModels are the author models a land release of the node recorded: its authors for crw.review. A land one of whose named
// authors was not read back has none (post-evaluation d2): that author may be of the reviewer's family, so the observed others do not
// prove the review independent, and crw.review answers independence_unproven.
func (r rows) landAuthorModels(node string) []string {
	var out []string
	for _, ev := range r.opRows(OpRelease) {
		if dataStr(ev, "node") != node || dataStr(ev, "kind") != "land" || !dataBool(ev, "released") {
			continue
		}
		var models []string
		unobserved := false
		list, _ := ev.Data["authors"].([]any)
		for _, item := range list {
			if a, ok := item.(map[string]any); ok {
				if flag(a, "observed") && str(a, "model") != "" {
					models = append(models, str(a, "model"))
				} else {
					unobserved = true
				}
			}
		}
		// the authors of the data before the JSON round trip of a stored row
		if typed, ok := ev.Data["authors"].([]LandAuthor); ok {
			for _, a := range typed {
				if a.Observed && a.Model != "" {
					models = append(models, a.Model)
				} else {
					unobserved = true
				}
			}
		}
		if !unobserved {
			out = append(out, models...)
		}
	}
	return out
}
