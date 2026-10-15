package ops

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/audit"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// crw.land is premerge assembly, then the agent path of dag-accept, then dag-integrate (R05): the lane's land (workflows/lib/lane.js)
// calls crw.accept and crw.integrate in that order, and the premerge record is assembled inside crw.accept from the verdict the
// run holds. A refusal of either is an op error that carries a closed reason (Refusal) and leaves a refusal row, not a completed
// row, so nothing that reads a completed crw.accept row (the engine's accepted-node check, AcceptProof) takes it for an acceptance.

// ---- crw.accept ----

// acceptOp is crw.accept(node, head): the acceptance of a head that was verified and reviewed. It needs the passing verification
// record and the verified review of that head in the journal, assembles the premerge record from the verdict, judges it as the
// relay's acceptance will (a finding that needs a disposition is blocking by default and the node waits for the orchestrator), and
// asks the relay to accept.
type acceptOp struct{ l *Lane }

// AcceptOutcome is the answer of crw.accept.
type AcceptOutcome struct {
	AcceptanceID   string `json:"acceptance_id"`
	Head           string `json:"head"`
	PremergeDigest string `json:"premerge_digest"`
	Replayed       bool   `json:"replayed"`
}

// AcceptOpts are the options of crw.accept: Carried are the ids of the findings the node lane carried past its last unit-check round
// trip (CRW-1465). Each is read from the node's recorded unit checks and joins the premerge record's defects when the check the
// acceptance rests on does not name it: after a fix of a failed verification that check judges only the fix, and the carried finding
// would be missing from the record. An id no check of the node recorded is an error.
type AcceptOpts struct {
	Carried []string `json:"carried"`
}

func (o *acceptOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var head HeadRef
	var opts AcceptOpts
	if err := args(req.Args, &node, &head, &opts); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.accept(node, head, opts): %w", err)
	}
	r := rows(req.Journal.Events())
	rel, ok := releaseRowOf(r, node)
	if !ok {
		return engine.OpResult{}, &Refusal{"node_not_released", "node " + node + " has no release in the journal"}
	}
	plan := dataStr(rel, "plan")
	ver, ok := r.opRows(OpVerify).last(func(ev engine.Event) bool {
		return dataStr(ev, "node") == node && dataStr(ev, "head") == head.Commit && dataBool(ev, "ok")
	})
	if !ok {
		return engine.OpResult{}, &Refusal{"verification_missing", "no passing verification of " + head.Commit + " is recorded for node " + node}
	}
	// The basis of the premerge record is the newer, in journal order, of the last verified review of the head and the last unit
	// check on the head that gave a judgment (CRW-1456): a check that gave none (no answer, quota, a failed tool, a changed
	// worktree, an invalid answer) is no basis. A later verdict of either kind that withdraws a verified one is the relay's to read.
	revAt := -1
	for i := len(r) - 1; i >= 0; i-- {
		if ev := r[i]; ev.Kind == engine.KindCall && ev.Op == OpReview && ev.Event == engine.EvCompleted && dataStr(ev, "node") == node && dataStr(ev, "head") == head.Commit &&
			dataStr(ev, "state") == StateVerdict && dataStr(ev, "verdict") == "verified" {
			revAt = i
			break
		}
	}
	// The tree of the head, which a check run before the commit names in place of a commit, is read from the journal first: the
	// commit row of the head, else the land release of the head, else git in the node's worktree. With none of them the tree is
	// unknown and such a check is on no head; it is resolved once, and only for such a check.
	tree, treeRead := "", false
	headTree := func() (string, error) {
		if treeRead {
			return tree, nil
		}
		treeRead = true
		if c, ok := r.opRows(OpCommit).last(func(ev engine.Event) bool {
			return dataStr(ev, "node") == node && dataStr(ev, "commit") == head.Commit && dataStr(ev, "tree") != ""
		}); ok {
			tree = dataStr(c, "tree")
		} else if dataStr(rel, "kind") == "land" && dataStr(rel, "head") == head.Commit && dataStr(rel, "tree") != "" {
			tree = dataStr(rel, "tree")
		} else if wt := r.worktreeOf(node); wt != "" {
			t, err := TreeOf(ctx, o.l.d.Git, wt, head.Commit)
			if err != nil {
				return "", err
			}
			tree = t
		}
		return tree, nil
	}
	ucAt := -1
	var uc reviewadapter.Result
	for i := len(r) - 1; i > revAt; i-- {
		ev := r[i]
		if ev.Kind != engine.KindCall || ev.Op != OpUnitCheck || ev.Event != engine.EvCompleted || dataStr(ev, "node") != node || dataStr(ev, "role") != execution.AgentUnitCheck {
			continue
		}
		var res reviewadapter.Result
		if json.Unmarshal(resultOf(ev), &res) != nil || res.State != reviewadapter.StateOK {
			continue
		}
		commit := dataStr(ev, "commit")
		if commit == "" {
			commit = dataStr(ev, "head")
		}
		onHead := commit == head.Commit
		if commit == "" && head.Commit != "" {
			t, err := headTree()
			if err != nil {
				return engine.OpResult{}, err
			}
			onHead = t != "" && dataStr(ev, "tree") == t
		}
		if onHead {
			ucAt, uc = i, res
			break
		}
	}
	var verdict ReviewVerdict
	var grader premerge.Grader
	var criteriaDigest, reviewerCall, basis string
	switch {
	case ucAt >= 0:
		row := r[ucAt]
		receipt := ""
		if rc, ok := row.Data["receipt"].(map[string]any); ok && str(rc, "state") == "accepted" {
			receipt = str(rc, "sha256")
		}
		v, err := UnitCheckVerdict(uc, row.CallID, dataStr(ver, "digest"), expectedCriteria(r, node), receipt)
		if err != nil {
			return engine.OpResult{}, &Refusal{"reviewer_not_verified", err.Error()}
		}
		verdict, grader = v, premerge.Grader{Model: uc.Model, Effort: uc.Effort, PromptDigest: uc.PromptDigest}
		criteriaDigest, reviewerCall, basis = dataStr(row, "criteria_digest"), row.CallID, audit.BasisUnitCheck
	case revAt >= 0:
		var rr ReviewResult
		if err := json.Unmarshal(resultOf(r[revAt]), &rr); err != nil {
			return engine.OpResult{}, err
		}
		v, err := DecodeVerdict(rr.Verdict)
		if err != nil {
			return engine.OpResult{}, &Refusal{"reviewer_not_verified", err.Error()}
		}
		prep, ok := r.prepared().last(func(ev engine.Event) bool { return ev.CallID == rr.Call.CallID })
		if !ok {
			return engine.OpResult{}, &Refusal{"reviewer_missing", "the reviewer call " + rr.Call.CallID + " has no prepared row"}
		}
		verdict, grader = v, premerge.Grader{Model: str(prep.Data, "model"), Effort: str(prep.Data, "effort"), PromptDigest: str(prep.Data, "prompt_digest")}
		criteriaDigest, reviewerCall, basis = rr.CriteriaDigest, rr.Call.CallID, audit.BasisReview
	default:
		return engine.OpResult{}, &Refusal{"reviewer_missing", "no verified review or unit check of " + head.Commit + " is recorded for node " + node}
	}
	if len(opts.Carried) > 0 {
		extra, err := carriedDefects(r, node, opts.Carried, verdict.Defects)
		if err != nil {
			return engine.OpResult{}, err
		}
		verdict.Defects = append(verdict.Defects, extra...)
		verdict.Summary += "; carried " + strings.Join(opts.Carried, ", ")
	}
	issue := node
	if is, found, err := issueOf(r, rel, node); err == nil && found {
		issue = is.Issue
	} else if pn, ok := planNodeOf(r, plan, dataStr(rel, "milestone"), node); ok && pn.Issue != "" {
		// a node released with no DESIGN (a head made outside a run, CRW-1354): the issue the plan gives it
		issue = pn.Issue
	}
	// The acceptance judges the head over the base its verification stood on: the commit the node's work grew from, an ancestor of
	// the head that does not move when another node lands (the tip of the integration target, sampled now, is neither). A
	// verification row that names none is judged over the base found now.
	base := dataStr(ver, "base")
	if base == "" {
		var err error
		if base, err = o.l.stableBase(ctx, plan, r.worktreeOf(node), head.Commit); err != nil {
			return engine.OpResult{}, err
		}
	}
	rec, err := AssemblePremerge(PremergeInput{
		Issue: issue, Node: node, Head: head.Commit, Dev: base, CriteriaDigest: criteriaDigest, Expect: expectedCriteria(r, node),
		Grader: grader, GradedAt: o.l.d.Now(), Verdict: verdict, By: o.l.d.Actor,
	})
	if err != nil {
		return engine.OpResult{}, &Refusal{"premerge_missing", err.Error()}
	}
	doc, reason, detail, err := PremergeVerdict(rec)
	if err != nil {
		return engine.OpResult{}, err
	}
	if reason != "" {
		return engine.OpResult{}, &Refusal{reason, detail}
	}
	var record json.RawMessage
	if s := dataStr(ver, "record"); s != "" {
		record = json.RawMessage(s)
	}
	res, err := o.l.d.Relay.Accept(ctx, AcceptRequest{Plan: plan, Node: node, Actor: o.l.d.Actor, RunID: req.RunID, Head: head.Commit, Base: base, Premerge: doc, Verification: record,
		CriteriaDigest: criteriaDigest, ReviewerCall: reviewerCall, RequestID: req.RequestID})
	if err != nil {
		return engine.OpResult{}, err
	}
	if !res.Accepted {
		return engine.OpResult{}, &Refusal{res.Reason, res.Detail}
	}
	pdigest, _ := premerge.Digest(doc)
	out := AcceptOutcome{AcceptanceID: res.AcceptanceID, Head: head.Commit, PremergeDigest: pdigest, Replayed: res.Replayed}
	// the row names what the acceptance rests on (basis, and reviewer_call: the reviewer's call or the crw.unitCheck row's), which
	// the audit of the milestone close reads
	return engine.OpResult{Result: result(out), Data: map[string]any{"node": node, "head": head.Commit, "acceptance_id": res.AcceptanceID, "premerge_digest": pdigest, "plan": plan,
		"basis": basis, "reviewer_call": reviewerCall}}, nil
}

// carriedDefects are the defects of the carried findings ids that have not a defect among those yet: each is the finding of that id in
// the newest unit check of the node that recorded it (role unit-check, a judgment), as UnitCheckVerdict turns a finding into a defect.
func carriedDefects(r rows, node string, ids []string, have []Defect) ([]Defect, error) {
	var out []Defect
	for _, id := range ids {
		if slices.ContainsFunc(have, func(d Defect) bool { return d.Ref == id }) || slices.ContainsFunc(out, func(d Defect) bool { return d.Ref == id }) {
			continue
		}
		var found *reviewadapter.Finding
		for i := len(r) - 1; i >= 0 && found == nil; i-- {
			ev := r[i]
			if ev.Kind != engine.KindCall || ev.Op != OpUnitCheck || ev.Event != engine.EvCompleted || dataStr(ev, "node") != node || dataStr(ev, "role") != execution.AgentUnitCheck {
				continue
			}
			var res reviewadapter.Result
			if json.Unmarshal(resultOf(ev), &res) != nil || res.State != reviewadapter.StateOK {
				continue
			}
			for j := range res.Findings {
				if res.Findings[j].ID == id {
					found = &res.Findings[j]
					break
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("crw.accept: node %s: the carried finding %s is in no recorded unit check of the node", node, id)
		}
		out = append(out, Defect{Ref: found.ID, Kind: "other", Severity: found.Severity, Impact: found.Impact, Introduced: true, File: found.File, Line: found.Line, Finding: found.Text})
	}
	return out, nil
}

// issueOf is the DESIGN entry of a node, from the ledger revision the node was released under.
func issueOf(r rows, rel engine.Event, node string) (DesignIssue, bool, error) {
	led, ok := ledgerOf(r, dataStr(rel, "milestone"), dataStr(rel, "revision"))
	if !ok {
		return DesignIssue{}, false, nil
	}
	return designIssue(led, node)
}

func (o *acceptOp) Proven(ctx context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	st, found, err := o.l.d.Relay.AcceptState(ctx, dataStr(row, "acceptance_id"))
	if err != nil {
		return false, "", err
	}
	switch {
	case !found:
		return false, "the relay has no such acceptance", nil
	case !st.Active:
		return false, "the acceptance is not active", nil
	case st.Stale:
		return false, "the acceptance is stale", nil
	case st.Head != dataStr(row, "head"):
		return false, "the acceptance stands on another head", nil
	}
	return true, "", nil
}

// ---- crw.integrate ----

// integrateOp is crw.integrate(node, head, acceptanceId): dag-integrate of an accepted head onto the integration target. The
// batch commit it records is what its proof reads: it stays an ancestor of the target however far the target moves on.
type integrateOp struct{ l *Lane }

// IntegrateOutcome is the answer of crw.integrate.
type IntegrateOutcome struct {
	BatchCommit    string `json:"batch_commit"`
	IntegrationRef string `json:"integration_ref"`
	Tip            string `json:"tip"`
	Head           string `json:"head"`
	Replayed       bool   `json:"replayed"`
}

func (o *integrateOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node, acceptance string
	var head HeadRef
	if err := args(req.Args, &node, &head, &acceptance); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.integrate(node, head, acceptanceId): %w", err)
	}
	r := rows(req.Journal.Events())
	rel, ok := releaseRowOf(r, node)
	if !ok {
		return engine.OpResult{}, &Refusal{"node_not_released", "node " + node + " has no release in the journal"}
	}
	res, err := o.l.d.Relay.Integrate(ctx, IntegrateRequest{Plan: dataStr(rel, "plan"), Node: node, Actor: o.l.d.Actor, RunID: req.RunID, AcceptanceID: acceptance, Head: head.Commit, RequestID: req.RequestID})
	if err != nil {
		return engine.OpResult{}, err
	}
	if !res.Landed {
		return engine.OpResult{}, &Refusal{res.Reason, res.Detail}
	}
	out := IntegrateOutcome{BatchCommit: res.BatchCommit, IntegrationRef: res.IntegrationRef, Tip: res.Tip, Head: head.Commit, Replayed: res.Replayed}
	return engine.OpResult{Result: result(out), Data: map[string]any{
		"node": node, "head": head.Commit, "acceptance_id": acceptance, "batch_commit": res.BatchCommit, "integration_ref": res.IntegrationRef, "tip": res.Tip, "plan": dataStr(rel, "plan"),
	}}, nil
}

func (o *integrateOp) Proven(ctx context.Context, _ []engine.Event, row engine.Event) (bool, string, error) {
	tip, err := o.l.d.Relay.IntegrationTip(ctx, dataStr(row, "plan"))
	if err != nil {
		return false, "", err
	}
	anc, err := IsAncestor(ctx, o.l.d.Git, o.l.d.Repo, dataStr(row, "batch_commit"), tip)
	if err != nil {
		return false, "", err
	}
	if !anc {
		return false, "the batch commit is not on the integration target", nil
	}
	return true, "", nil
}
