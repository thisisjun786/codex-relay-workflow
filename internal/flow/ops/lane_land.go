package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
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

func (o *acceptOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var head HeadRef
	if err := args(req.Args, &node, &head); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.accept(node, head): %w", err)
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
	rev, ok := r.opRows(OpReview).last(func(ev engine.Event) bool {
		return dataStr(ev, "node") == node && dataStr(ev, "head") == head.Commit && dataStr(ev, "state") == StateVerdict && dataStr(ev, "verdict") == "verified"
	})
	if !ok {
		return engine.OpResult{}, &Refusal{"reviewer_missing", "no verified review of " + head.Commit + " is recorded for node " + node}
	}
	var rr ReviewResult
	if err := json.Unmarshal(resultOf(rev), &rr); err != nil {
		return engine.OpResult{}, err
	}
	verdict, err := DecodeVerdict(rr.Verdict)
	if err != nil {
		return engine.OpResult{}, &Refusal{"reviewer_not_verified", err.Error()}
	}
	prep, ok := r.prepared().last(func(ev engine.Event) bool { return ev.CallID == rr.Call.CallID })
	if !ok {
		return engine.OpResult{}, &Refusal{"reviewer_missing", "the reviewer call " + rr.Call.CallID + " has no prepared row"}
	}
	issue := node
	if is, found, err := issueOf(r, rel, node); err == nil && found {
		issue = is.Issue
	}
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	rec, err := AssemblePremerge(PremergeInput{
		Issue: issue, Node: node, Head: head.Commit, Dev: tip, CriteriaDigest: rr.CriteriaDigest,
		Grader:   premerge.Grader{Model: str(prep.Data, "model"), Effort: str(prep.Data, "effort"), PromptDigest: str(prep.Data, "prompt_digest")},
		GradedAt: o.l.d.Now(), Verdict: verdict, By: o.l.d.Actor,
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
	res, err := o.l.d.Relay.Accept(ctx, AcceptRequest{Plan: plan, Node: node, Actor: o.l.d.Actor, RunID: req.RunID, Head: head.Commit, Base: tip, Premerge: doc, Verification: record,
		CriteriaDigest: rr.CriteriaDigest, ReviewerCall: rr.Call.CallID, RequestID: req.RequestID})
	if err != nil {
		return engine.OpResult{}, err
	}
	if !res.Accepted {
		return engine.OpResult{}, &Refusal{res.Reason, res.Detail}
	}
	pdigest, _ := premerge.Digest(doc)
	out := AcceptOutcome{AcceptanceID: res.AcceptanceID, Head: head.Commit, PremergeDigest: pdigest, Replayed: res.Replayed}
	return engine.OpResult{Result: result(out), Data: map[string]any{"node": node, "head": head.Commit, "acceptance_id": res.AcceptanceID, "premerge_digest": pdigest, "plan": plan}}, nil
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
