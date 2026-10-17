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

// AcceptOpts are the options of crw.accept: Carried are the findings the node lane carried past its last unit-check round trip
// (CRW-1465), each by its id and the crw.unitCheck call that carried it. Each is read from that check's record and joins the premerge
// record's defects when the check the acceptance rests on does not name it: after a fix of a failed verification that check judges only
// the fix, and the carried finding would be missing from the record. The carrying check is named because a finding id is a file and
// a text and no grade: a later check may have reported the same finding at another severity. A finding the named check of the node did
// not record is an error.
type AcceptOpts struct {
	Carried []CarriedFinding `json:"carried"`
}

// CarriedFinding is a finding the node lane carried: its finding id and the call id of the unit check that carried it.
type CarriedFinding struct {
	ID    string `json:"id"`
	Check string `json:"check"`
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
	is, hasIssue, err := issueOf(r, rel, node)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.accept: node %s: its released DESIGN: %w", node, err)
	}
	wholeRangeOnly := hasIssue && is.SizeFit == "split" && len(is.SubTasks) > 0
	ver, ok := r.opRows(OpVerify).last(func(ev engine.Event) bool {
		return dataStr(ev, "node") == node && dataStr(ev, "head") == head.Commit && dataBool(ev, "ok")
	})
	if !ok {
		return engine.OpResult{}, &Refusal{"verification_missing", "no passing verification of " + head.Commit + " is recorded for node " + node}
	}
	// The tree of the head, which a check run before the commit names in place of a commit, is read from the journal first: the
	// commit row of the head, else the land release of the head, else git in the node's worktree. With none of them the tree is
	// unknown and such a check is on no head; it is resolved once, and only for such a check.
	tree, treeRead := "", false
	headTree := func() string {
		if treeRead {
			return tree
		}
		treeRead = true
		if c, ok := r.opRows(OpCommit).last(func(ev engine.Event) bool {
			return dataStr(ev, "node") == node && dataStr(ev, "commit") == head.Commit && dataStr(ev, "tree") != ""
		}); ok {
			tree = dataStr(c, "tree")
		} else if dataStr(rel, "kind") == "land" && dataStr(rel, "head") == head.Commit && dataStr(rel, "tree") != "" {
			tree = dataStr(rel, "tree")
		} else if wt := r.worktreeOf(node); wt != "" {
			// A failed lookup leaves the tree unknown; it cannot bind a tree-only
			// check to this head or invalidate a review that already names the head.
			if t, err := TreeOf(ctx, o.l.d.Git, wt, head.Commit); err == nil {
				tree = t
			}
		}
		return tree
	}
	// The basis of the premerge record is the newest judgment of the head in journal order (CRW-1456, CRW-1480): a review of the head
	// with a verdict of either value, or a unit check on the head that gave a judgment. A check that gave none (no answer, quota, a
	// failed tool, a changed worktree, an invalid answer) is no judgment, and an older judgment is withdrawn by a newer one: a newest
	// review that needs changes refuses the acceptance here, before the relay is asked. A sha names the same object in either case,
	// as the relay's acceptance reads it, so the head and tree of a judgment are compared with strings.EqualFold.
	judgedAt := -1
	var uc reviewadapter.Result
	for i := len(r) - 1; i >= 0 && judgedAt < 0; i-- {
		ev := r[i]
		if ev.Kind != engine.KindCall || ev.Event != engine.EvCompleted || dataStr(ev, "node") != node {
			continue
		}
		switch ev.Op {
		case OpReview:
			if dataStr(ev, "state") == StateVerdict && strings.EqualFold(dataStr(ev, "head"), head.Commit) {
				judgedAt = i
			}
		case OpUnitCheck:
			if dataStr(ev, "role") != execution.AgentUnitCheck {
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
			onHead := strings.EqualFold(commit, head.Commit)
			if commit == "" && head.Commit != "" {
				t := headTree()
				onHead = t != "" && strings.EqualFold(dataStr(ev, "tree"), t)
			}
			if onHead {
				// An uncommitted check covers only this sub-task's pending change. A verified result cannot approve a
				// split node; a matching negative judgment still refuses through the usual path below.
				if wholeRangeOnly && dataStr(ev, "mode") != UnitCheckCommitted && res.Verdict == reviewadapter.VerdictVerified {
					continue
				}
				judgedAt, uc = i, res
			}
		}
	}
	var verdict ReviewVerdict
	var grader premerge.Grader
	var criteriaDigest, reviewerCall, basis string
	switch {
	case judgedAt >= 0 && r[judgedAt].Op == OpUnitCheck:
		row := r[judgedAt]
		// the receipt the check read, and whether the tool that judged was given it (CRW-1517); a row with no handed is from before
		// it was recorded, when every unit check took the prompt
		var receipt UnitCheckReceipt
		if rc, ok := row.Data["receipt"].(map[string]any); ok && str(rc, "state") == "accepted" {
			receipt = UnitCheckReceipt{SHA256: str(rc, "sha256"), Handed: true}
			if h, has := rc["handed"]; has {
				b, ok := h.(bool)
				if !ok {
					return engine.OpResult{}, &Refusal{"reviewer_not_verified", fmt.Sprintf("unit check %s records a receipt handed %v (%T), not a bool", row.CallID, h, h)}
				}
				receipt.Handed = b
			}
		}
		v, err := UnitCheckVerdict(uc, row.CallID, dataStr(ver, "digest"), expectedCriteria(r, node), receipt)
		if err != nil {
			return engine.OpResult{}, &Refusal{"reviewer_not_verified", err.Error()}
		}
		verdict, grader = v, premerge.Grader{Model: uc.Model, Effort: uc.Effort, PromptDigest: uc.PromptDigest}
		criteriaDigest, reviewerCall, basis = dataStr(row, "criteria_digest"), row.CallID, audit.BasisUnitCheck
	case judgedAt >= 0:
		row := r[judgedAt]
		if v := dataStr(row, "verdict"); v != "verified" {
			return engine.OpResult{}, &Refusal{"reviewer_not_verified", fmt.Sprintf("the newest judgment of %s for node %s is review %s (reviewer call %s), which ruled %q, not verified",
				head.Commit, node, row.CallID, dataStr(row, "call"), v)}
		}
		var rr ReviewResult
		if err := json.Unmarshal(resultOf(row), &rr); err != nil {
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
	if dataStr(rel, "kind") == "land" && !landAuthorsObserved(rel) {
		// Keep the original authors' missing readback visible; a later repair does not fill it in.
		// It is provenance, not a model-family gate (CRW-1515).
		verdict.Summary += "; original land author metadata was not fully observed"
	}
	if len(opts.Carried) > 0 {
		extra, err := carriedDefects(r, node, opts.Carried, verdict.Defects)
		if err != nil {
			return engine.OpResult{}, err
		}
		verdict.Defects = append(verdict.Defects, extra...)
		ids := make([]string, 0, len(opts.Carried))
		for _, c := range opts.Carried {
			ids = append(ids, c.ID)
		}
		verdict.Summary += "; carried " + strings.Join(ids, ", ")
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

// carriedDefects are the defects of the carried findings that have not a defect among those yet: each is the finding of its id in the
// record of the unit check that carried it (a completed crw.unitCheck row of the node with that call id, role unit-check, a
// judgment), as UnitCheckVerdict turns a finding into a defect.
func carriedDefects(r rows, node string, carried []CarriedFinding, have []Defect) ([]Defect, error) {
	var out []Defect
	for _, c := range carried {
		id := c.ID
		if slices.ContainsFunc(have, func(d Defect) bool { return d.Ref == id }) || slices.ContainsFunc(out, func(d Defect) bool { return d.Ref == id }) {
			continue
		}
		var found *reviewadapter.Finding
		if row, ok := r.opRows(OpUnitCheck).last(func(ev engine.Event) bool {
			return ev.CallID == c.Check && dataStr(ev, "node") == node && dataStr(ev, "role") == execution.AgentUnitCheck
		}); ok && c.Check != "" {
			var res reviewadapter.Result
			if json.Unmarshal(resultOf(row), &res) == nil && res.State == reviewadapter.StateOK {
				for j := range res.Findings {
					if res.Findings[j].ID == id {
						found = &res.Findings[j]
						break
					}
				}
			}
		}
		if found == nil {
			return nil, fmt.Errorf("crw.accept: node %s: the carried finding %s is not in the record of unit check %q of the node", node, id, c.Check)
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
