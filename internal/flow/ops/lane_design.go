package ops

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/execution"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// ---- crw.milestone ----

// milestoneOp is crw.milestone(plan, milestone): the milestone's nodes in the order the scheduler offers them, with the edges and
// the edit regions of each.
type milestoneOp struct{ l *Lane }

func (o *milestoneOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var plan, id string
	if err := args(req.Args, &plan, &id); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.milestone(plan, milestone): %w", err)
	}
	if plan == "" || id == "" {
		return engine.OpResult{}, fmt.Errorf("crw.milestone(plan, milestone): a plan and a milestone are required")
	}
	ms, err := o.l.liveMilestone(ctx, plan, id)
	if err != nil {
		return engine.OpResult{}, err
	}
	return engine.OpResult{Result: result(ms), Data: map[string]any{"plan": plan, "milestone": id, "nodes": len(ms.Nodes), "digest": digestOf(ms)}}, nil
}

// normalizeMilestone gives every list of a milestone its empty value, so the JSON a script reads and the digest of the plan do not depend
// on whether the relay answered null or an empty list.
func normalizeMilestone(ms *Milestone) {
	for i := range ms.Nodes {
		n := &ms.Nodes[i]
		if n.After == nil {
			n.After = []string{}
		}
		if n.Regions == nil {
			n.Regions = []string{}
		}
		if n.Criteria == nil {
			n.Criteria = []string{}
		}
	}
	if ms.Nodes == nil {
		ms.Nodes = []PlanNode{}
	}
}

func (o *milestoneOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.designAudit ----

// The states of a design audit and of a review: the one state a result is in, and the only state that moves the lane on.
const (
	AuditApproved     = "approved"
	AuditNeedsChanges = "needs_changes"
	StateVerdict      = "verdict"
	StateHeld         = "held"
	StateUnknown      = "unknown"
	StateRefused      = "refused"
	StateNullModel    = "null_model"
	StateSuperseded   = "superseded"
)

// Candidate is a DESIGN with the answers of its open questions: what the audit judges and the ledger records.
type Candidate struct {
	Design  json.RawMessage `json:"design"`
	Answers json.RawMessage `json:"answers"`
}

func (c Candidate) digest() string {
	var design, answers any
	_ = json.Unmarshal(c.Design, &design)
	if len(c.Answers) > 0 {
		_ = json.Unmarshal(c.Answers, &answers)
	}
	if answers == nil {
		answers = []any{}
	}
	return digestOf(map[string]any{"design": design, "answers": answers})
}

// independent reports why a call of the family of reviewer is not an independent second opinion of the models: "" when its
// model family differs from every one of theirs, else the closed reason. A pair that was never read back proves nothing.
func independent(reviewer string, authors []string) string {
	if reviewer == "" || len(authors) == 0 {
		return "independence_unproven"
	}
	for _, a := range authors {
		if a == "" || execution.ModelFamily(a) == execution.ModelFamily(reviewer) {
			return engine.ReasonIndependenceRefused
		}
	}
	return ""
}

// designAuditOp is crw.designAudit(milestone, round, candidate, architectCall, auditCall): the record of the independent audit of
// a DESIGN. The auditor is a reviewer of another model family than the architect; only an approved audit lets the DESIGN into the
// ledger, and the executors start only on a ledger revision. A call that did not complete is not an audit: held and unknown hold
// the milestone, refused escalates, null_model asks the auditor again once, superseded stops.
type designAuditOp struct{ l *Lane }

// AuditResult is the answer of crw.designAudit.
type AuditResult struct {
	State           string         `json:"state"`
	Reason          string         `json:"reason"`
	Round           int            `json:"round"`
	CandidateDigest string         `json:"candidate_digest"`
	Findings        []AuditFinding `json:"findings"`
	Summary         string         `json:"summary"`
	Reviewer        Brief          `json:"reviewer"`
	Architect       Brief          `json:"architect"`
}

func (o *designAuditOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var round int
	var cand Candidate
	var arch, call CallRef
	if err := args(req.Args, &ms, &round, &cand, &arch, &call); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designAudit(milestone, round, candidate, architect, call): %w", err)
	}
	if _, err := DecodeDesign(cand.Design); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designAudit: the candidate is no DESIGN: %w", err)
	}
	res := AuditResult{Round: round, CandidateDigest: cand.digest(), Findings: []AuditFinding{}, Reviewer: call.brief(), Architect: arch.brief()}
	switch call.Status {
	case "completed":
		audit, err := DecodeDesignAudit(call.Output)
		if err != nil {
			res.State, res.Reason = StateNullModel, "audit_invalid"
			break
		}
		res.Findings, res.Summary = audit.Findings, audit.Summary
		if audit.Findings == nil {
			res.Findings = []AuditFinding{}
		}
		if why := independent(call.Meta.ConfiguredPair.Model, []string{arch.Meta.ConfiguredPair.Model}); why != "" {
			res.State, res.Reason = StateRefused, why
			break
		}
		res.State = audit.Verdict
	case "null_model":
		res.State, res.Reason = StateNullModel, call.Reason
	case "held":
		res.State, res.Reason = StateHeld, call.Reason
	case "unknown":
		res.State, res.Reason = StateUnknown, call.Reason
	case "refused":
		res.State, res.Reason = StateRefused, call.Reason
	default:
		res.State, res.Reason = StateSuperseded, call.Reason
	}
	return engine.OpResult{Result: result(res), Data: map[string]any{
		"milestone": ms, "round": round, "candidate_digest": res.CandidateDigest, "state": res.State, "reason": res.Reason,
		"reviewer_call": call.CallID, "architect_call": arch.CallID, "reviewer_model": call.Meta.ConfiguredPair.Model, "architect_model": arch.Meta.ConfiguredPair.Model,
	}}, nil
}

func (o *designAuditOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// approvedAudit is the approved audit row of the milestone for a candidate digest.
func approvedAudit(r rows, ms, digest string) (engine.Event, bool) {
	return r.opRows(OpDesignAudit).last(func(ev engine.Event) bool {
		return dataStr(ev, "milestone") == ms && dataStr(ev, "candidate_digest") == digest && dataStr(ev, "state") == AuditApproved
	})
}

// ---- crw.designLedger ----

// FirstRevision is the first revision of a milestone's ledger. Revisions after it are made by a re-plan (replan.go): the ledger adds
// one for every architect delta that passed the design audit again.
const FirstRevision = "r0"

// designLedgerOp is crw.designLedger(milestone, entry): it journals a revision of the milestone's design ledger - the DESIGN that
// passed the audit with the answers of its open questions, the digest the packets and the input snapshots carry, the handle of the
// architect thread (the thread goes on with every completed architect turn) and the slice digest of every node of the plan the
// revision was made against. The first revision is r0; a revision after it is the architect's delta after a re-plan signal, and is
// recorded only over an audit of its own that was approved after the revision before it.
type designLedgerOp struct{ l *Lane }

// LedgerEntry is what a script hands crw.designLedger.
type LedgerEntry struct {
	// Revision is the id of the revision the entry becomes; empty is the next one. A script that names one names the next.
	Revision  string          `json:"revision"`
	Design    json.RawMessage `json:"design"`
	Answers   json.RawMessage `json:"answers"`
	Architect CallRef         `json:"architect"`
	// Signal is the number of the signal that made the revision (0 for the first design); Head is the head of the integration
	// target the architect judged for a revision after the first.
	Signal int    `json:"signal"`
	Head   string `json:"head"`
	// Plan is the plan the architect and the auditor evaluated for a revision after the first (the plan of crw.replanSignals): the
	// revision records its slices, so that a plan revision made while the delta was architected or audited is a change against the
	// revision, counted by the next signal, and not part of its baseline. Without it the revision records the plan as it is now.
	Plan *PlanSnapshot `json:"plan"`
}

// Ledger is the answer of crw.designLedger and the shape of the revision a release reads back.
type Ledger struct {
	Milestone string   `json:"milestone"`
	Revision  string   `json:"revision"`
	Digest    string   `json:"digest"`
	Handle    string   `json:"handle"`
	Nodes     []string `json:"nodes"`
	BaseTip   string   `json:"base_tip"`
}

func (o *designLedgerOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var ms string
	var e LedgerEntry
	if err := args(req.Args, &ms, &e); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger(milestone, entry): %w", err)
	}
	r := rows(req.Journal.Events())
	next := nextRevision(r, ms)
	if e.Revision == "" {
		e.Revision = next
	}
	if e.Revision != next {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: revision %s: the ledger of milestone %s adds %s next", e.Revision, ms, next)
	}
	prev, hasPrev := currentLedger(r, ms)
	if e.Revision == FirstRevision && hasPrev {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: milestone %s has revision %s already", ms, dataStr(prev, "revision"))
	}
	if e.Revision != FirstRevision && !hasPrev {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: revision %s: milestone %s has no revision it could follow", e.Revision, ms)
	}
	design, err := DecodeDesign(e.Design)
	if err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: %w", err)
	}
	cand := Candidate{Design: e.Design, Answers: e.Answers}
	digest := cand.digest()
	// the audit of a revision after the first is an audit of its own, made after the revision before it: the audit of an earlier
	// DESIGN of the same digest stands in for nothing
	after := 0
	if hasPrev {
		after = prev.Seq
	}
	if _, ok := approvedAuditAfter(r, ms, digest, after); !ok {
		return engine.OpResult{}, fmt.Errorf("crw.designLedger: the DESIGN of milestone %s (%s) has no approved audit: executors start only on an audited revision", ms, short(digest))
	}
	plan := planOf(r, ms)
	tip, err := o.l.d.Relay.IntegrationTip(ctx, plan)
	if err != nil {
		return engine.OpResult{}, err
	}
	// the slices are those of the plan the architect saw: the milestone as the run read it for the first revision, the plan the
	// decision evaluated for the ones after (a re-plan after a plan revision starts counting the change from that revision, and a
	// change made after the evaluation is counted by the next signal)
	var mile Milestone
	var sl map[string]string
	var mileDigest string
	switch {
	case hasPrev && e.Plan != nil:
		sl, mileDigest = e.Plan.Slices, e.Plan.Digest
		if sl == nil {
			sl = map[string]string{}
		}
	case hasPrev:
		if mile, err = o.l.liveMilestone(ctx, plan, ms); err != nil {
			return engine.OpResult{}, err
		}
		sl, mileDigest = liveSlices(mile), digestOf(mile)
	default:
		if mile, _, err = milestoneOf(r, ms); err != nil {
			return engine.OpResult{}, err
		}
		sl, mileDigest = liveSlices(mile), digestOf(mile)
	}
	base := tip
	if hasPrev && e.Head != "" {
		base = e.Head
	}
	led := Ledger{Milestone: ms, Revision: e.Revision, Digest: digest, Handle: e.Architect.Handle, Nodes: []string{}, BaseTip: base}
	for _, is := range design.Issues {
		led.Nodes = append(led.Nodes, is.Node)
	}
	var answers any
	if len(e.Answers) > 0 {
		_ = json.Unmarshal(e.Answers, &answers)
	}
	var designAny any
	_ = json.Unmarshal(e.Design, &designAny)
	return engine.OpResult{Result: result(led), Data: map[string]any{
		"milestone": ms, "revision": e.Revision, "digest": digest, "architect_call": e.Architect.CallID, "architect_handle": e.Architect.Handle,
		"base_tip": base, "design": designAny, "answers": answers, "signal": e.Signal, "slices": sl, "milestone_digest": mileDigest,
	}}, nil
}

func (o *designLedgerOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ledgerOf is the ledger row of a milestone's revision.
func ledgerOf(r rows, ms, revision string) (engine.Event, bool) {
	return r.opRows(OpDesignLedger).last(func(ev engine.Event) bool {
		return dataStr(ev, "milestone") == ms && dataStr(ev, "revision") == revision
	})
}

// ---- crw.release ----

// releaseOp is crw.release(plan, node, milestone): the release of an issue node to an executor of the run, under the milestone's
// ledger revision. It refuses to release a node whose DESIGN has no approved audit - the structural guarantee that no executor
// starts before the audit passed - and a node whose issue the architect sized for a child or a split is not released to an
// executor at all (the lane releases the child or escalates). The row it writes names the revision and the digest: the packet of
// the lane is built from them, and the input snapshot of every call of the node carries them.
type releaseOp struct{ l *Lane }

// ReleaseOutcome is the answer of crw.release.
type ReleaseOutcome struct {
	Released     bool   `json:"released"`
	Reason       string `json:"reason"`
	Node         string `json:"node"`
	Kind         string `json:"kind"`
	Revision     string `json:"revision"`
	DesignDigest string `json:"design_digest"`
	ExecutionID  string `json:"execution_id"`
}

func (o *releaseOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var plan, node, ms string
	if err := args(req.Args, &plan, &node, &ms); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.release(plan, node, milestone): %w", err)
	}
	r := rows(req.Journal.Events())
	led, ok := currentLedger(r, ms)
	out := ReleaseOutcome{Node: node, Kind: "executor"}
	data := map[string]any{"plan": plan, "node": node, "milestone": ms}
	finish := func() (engine.OpResult, error) {
		data["released"], data["reason"], data["kind"] = out.Released, out.Reason, out.Kind
		return engine.OpResult{Result: result(out), Data: data}, nil
	}
	if !ok {
		out.Reason = "design_missing"
		return finish()
	}
	out.Revision, out.DesignDigest = dataStr(led, "revision"), dataStr(led, "digest")
	if _, audited := approvedAudit(r, ms, out.DesignDigest); !audited {
		out.Reason = "design_not_audited"
		return finish()
	}
	entry, found, err := designIssue(led, node)
	if err != nil {
		return engine.OpResult{}, err
	}
	if !found {
		out.Reason = "node_not_designed"
		return finish()
	}
	// A node that was released under a revision a resume superseded, whose slice of the plan is not the one the revision that stands
	// was made against, and that a landed acceptance rests on, is not released again: the route is a plan revision with update_node
	// or dag-correct, as for any accepted node whose spec changed.
	if old, was := releaseRowOf(r, node); was && revisionSuperseded(r, ms, dataStr(old, "revision")) && ledgerSlices(led)[node] != dataStr(old, "slice_digest") && acceptedAfter(r, node, old) {
		return engine.OpResult{}, &Refusal{"accepted_node_changed", fmt.Sprintf("node %s was accepted under revision %s, which a resume superseded, and its slice of the plan is not the one revision %s was made against: record a plan revision with update_node (the acceptance becomes stale) and run again, or correct an oversized child with dag-correct", node, dataStr(old, "revision"), out.Revision)}
	}
	data["revision"], data["design_digest"] = out.Revision, out.DesignDigest
	switch entry.SizeFit {
	case "child":
		out.Kind, out.Reason = "child", "size_fit_child"
		return finish()
	case "split":
		out.Kind, out.Reason = "split", "size_split"
		return finish()
	}
	// the criteria are those of the plan the run read; the slice is the node's slice of the plan as it is when the node is released,
	// the one a later revision of the ledger is compared with (a node whose slice another revision was not made against is not kept
	// under it)
	mnode, planned := planNodeOf(r, plan, ms, node)
	now, err := o.l.liveMilestone(ctx, plan, ms)
	if err != nil {
		return engine.OpResult{}, err
	}
	slice, live := liveSlices(now)[node]
	if !live && planned {
		// the plan the relay reads has dropped the node: nothing is released, and no slice of the plan the run read first stands in for
		// the one the node no longer has
		out.Reason = "node_not_in_plan"
		return finish()
	}
	data["slice_digest"], data["criteria_digest"] = slice, mnode.CriteriaDigest
	res, err := o.l.d.Relay.Release(ctx, ReleaseRequest{Plan: plan, Node: node, RunID: req.RunID, Revision: out.Revision, DesignDigest: out.DesignDigest, RequestID: req.RequestID})
	if err != nil {
		return engine.OpResult{}, err
	}
	out.Released, out.Reason, out.ExecutionID = res.Released, res.Reason, res.ExecutionID
	data["execution_id"], data["slot_id"], data["request_id"] = res.ExecutionID, res.SlotID, req.RequestID
	return finish()
}

// Proven: a release is proven while the relay says it stands. It is also proven when the run itself ended the execution after the
// release: it gave the node up (a completed crw.escalate row of the node that closed the execution) or landed it (a completed
// crw.integrate row of the node). A closed execution does not stand at the relay, and the lane that closed it must not be
// released and built again on a resume - the escalation is the node's decision until crw.decide answers it.
//
// A release made under a revision a resume superseded (the signal that made the revision was not replayed) stands only for a node
// whose slice of the plan is the one the revision that stands was made against: the replay of the lane is kept and a release_rebind
// row says so. For any other node the position runs again under the revision that stands - a node that had not started gets the new
// packet, and a node that had started and was not accepted runs again from its first position (the position is live, so is every
// later one of the lane); one that was accepted is refused by Run.
func (o *releaseOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	if !dataBool(row, "released") {
		// a node the architect sized for a child or a split was not released to an executor on purpose: the row is the decision
		// of the ledger revision it names, which does not change, and the child's start has a row of its own (crw.releaseChild).
		// Asking again would start the child again.
		if k := dataStr(row, "kind"); k == "child" || k == "split" {
			return true, "", nil
		}
		// a node the scheduler held back may be releasable now: the position asks again
		return false, "the release did not happen", nil
	}
	r := rows(ev)
	ms, rev, node := dataStr(row, "milestone"), dataStr(row, "revision"), dataStr(row, "node")
	if revisionSuperseded(r, ms, rev) {
		cur, ok := currentLedger(r, ms)
		if !ok || ledgerSlices(cur)[node] == "" || ledgerSlices(cur)[node] != dataStr(row, "slice_digest") {
			return false, "the release stands under revision " + rev + ", which a resume superseded, and the node's slice is not the one the revision that stands was made against", nil
		}
		o.l.rebind(r, row, cur)
	}
	if ended, how := endedByRun(r, row); ended {
		return true, how, nil
	}
	ok, err := o.l.d.Relay.ReleaseStands(ctx, dataStr(row, "plan"), dataStr(row, "node"), dataStr(row, "execution_id"))
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "the execution of the release is closed", nil
	}
	return true, "", nil
}

// endedByRun says whether the run ended the execution of a release: a later completed escalation of the node that closed it, or a
// later completed integration of the node.
func endedByRun(r rows, release engine.Event) (bool, string) {
	node := dataStr(release, "node")
	if _, ok := r.opRows(OpEscalate).last(func(ev engine.Event) bool {
		return ev.Seq > release.Seq && dataStr(ev, "node") == node && dataBool(ev, "closed")
	}); ok {
		return true, "the run closed the execution when it gave the node up"
	}
	if _, ok := r.opRows(OpIntegrate).last(func(ev engine.Event) bool { return ev.Seq > release.Seq && dataStr(ev, "node") == node }); ok {
		return true, "the run landed the node"
	}
	return false, ""
}

// planOf is the plan a milestone was read from.
func planOf(r rows, ms string) string {
	row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "milestone") == ms })
	if !ok {
		return ""
	}
	return dataStr(row, "plan")
}

// planNodeOf is the milestone's node as the milestone row of the journal recorded it.
func planNodeOf(r rows, plan, ms, node string) (PlanNode, bool) {
	row, ok := r.opRows(OpMilestone).last(func(ev engine.Event) bool { return dataStr(ev, "plan") == plan && dataStr(ev, "milestone") == ms })
	if !ok {
		return PlanNode{}, false
	}
	var m Milestone
	if json.Unmarshal(resultOf(row), &m) != nil {
		return PlanNode{}, false
	}
	for _, n := range m.Nodes {
		if n.ID == node {
			return n, true
		}
	}
	return PlanNode{}, false
}

// designIssue is the plan of a node in the DESIGN a ledger row holds.
func designIssue(led engine.Event, node string) (DesignIssue, bool, error) {
	raw, err := json.Marshal(led.Data["design"])
	if err != nil {
		return DesignIssue{}, false, err
	}
	d, err := DecodeDesign(raw)
	if err != nil {
		return DesignIssue{}, false, fmt.Errorf("the ledger holds no valid DESIGN: %w", err)
	}
	for _, is := range d.Issues {
		if is.Node == node {
			return is, true, nil
		}
	}
	return DesignIssue{}, false, nil
}

// releaseRowOf is the node's newest release row that released it.
func releaseRowOf(r rows, node string) (engine.Event, bool) {
	return r.opRows(OpRelease).last(func(ev engine.Event) bool { return dataStr(ev, "node") == node && dataBool(ev, "released") })
}

// designRowOf is the node's newest release row that names the design revision it was made under, whether it released the node to
// an executor or found it sized for a child or a split.
func designRowOf(r rows, node string) (engine.Event, bool) {
	return r.opRows(OpRelease).last(func(ev engine.Event) bool { return dataStr(ev, "node") == node && dataStr(ev, "revision") != "" })
}

// ---- crw.issuePlan ----

// issuePlanOp is crw.issuePlan(node): the plan of the node's issue from the ledger revision the node was released under. It reads
// the journal and nothing the script holds: a lane that reads the plan from a variable would build its packet from whatever
// revision the script saw last, and a replayed lane must build the packet it built.
type issuePlanOp struct{ l *Lane }

// IssuePlan is the plan of one issue, as a lane builds its packet from it.
type IssuePlan struct {
	DesignIssue
	Revision     string          `json:"revision"`
	DesignDigest string          `json:"design_digest"`
	Milestone    string          `json:"milestone"`
	Regions      []string        `json:"regions"`
	Decisions    []Decision      `json:"decisions"`
	Answers      json.RawMessage `json:"answers"`
	Title        string          `json:"title"`
	Criteria     []string        `json:"criteria"`
	// CriteriaDigest is the digest of the node's criteria set the plan fixed when the node was released: the digest a review and its
	// premerge record carry.
	CriteriaDigest string `json:"criteria_digest"`
}

func (o *issuePlanOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	if err := args(req.Args, &node); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan(node): %w", err)
	}
	r := rows(req.Journal.Events())
	rel, ok := designRowOf(r, node)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: node %s has no release in the journal", node)
	}
	ms, revision := dataStr(rel, "milestone"), dataStr(rel, "revision")
	led, ok := ledgerOf(r, ms, revision)
	if !ok {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: the ledger has no revision %s of milestone %s", revision, ms)
	}
	if dataStr(led, "digest") != dataStr(rel, "design_digest") {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: revision %s of milestone %s is not the one node %s was released under", revision, ms, node)
	}
	entry, found, err := designIssue(led, node)
	if err != nil {
		return engine.OpResult{}, err
	}
	if !found {
		return engine.OpResult{}, fmt.Errorf("crw.issuePlan: revision %s plans no issue for node %s", revision, node)
	}
	raw, _ := json.Marshal(led.Data["design"])
	design, _ := DecodeDesign(raw)
	plan := IssuePlan{DesignIssue: entry, Revision: revision, DesignDigest: dataStr(rel, "design_digest"), Milestone: ms, Regions: entry.EditRegions, Decisions: []Decision{}, Criteria: []string{},
		CriteriaDigest: dataStr(rel, "criteria_digest")}
	for _, ref := range entry.DecisionRefs {
		for _, d := range design.Decisions {
			if d.ID == ref {
				plan.Decisions = append(plan.Decisions, d)
			}
		}
	}
	if answers, err := json.Marshal(led.Data["answers"]); err == nil && string(answers) != "null" {
		plan.Answers = answers
	} else {
		plan.Answers = json.RawMessage(`[]`)
	}
	if n, ok := planNodeOf(r, dataStr(rel, "plan"), ms, node); ok {
		plan.Title, plan.Criteria = n.Title, n.Criteria
	}
	if plan.Regions == nil {
		plan.Regions = []string{}
	}
	return engine.OpResult{Result: result(plan), Data: map[string]any{"node": node, "milestone": ms, "revision": revision, "design_digest": plan.DesignDigest}}, nil
}

func (o *issuePlanOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- the input snapshot ----

// InputSnapshot is engine.Config.InputSnapshot: the facts only an external act changes, journaled with every call of a node and
// compared when the run resumes - the revision and digest of the DESIGN the node was released under, the node's slice of the
// plan revision and its criteria digest. A call that belongs to no released node has none. The facts are read from the release
// row, which is written once when the node is released, so a ledger that later grows a revision changes nothing for a node that
// started under r0.
func (l *Lane) InputSnapshot(_ context.Context, req engine.SnapshotRequest) (string, error) {
	if req.Node == "" || l.d.Journal == nil {
		return "", nil
	}
	rel, ok := releaseRowOf(rows(l.d.Journal.Events()), req.Node)
	if !ok {
		return "", nil
	}
	return fmt.Sprintf("design=%s:%s;slice=%s;criteria=%s", dataStr(rel, "revision"), dataStr(rel, "design_digest"), dataStr(rel, "slice_digest"), dataStr(rel, "criteria_digest")), nil
}
