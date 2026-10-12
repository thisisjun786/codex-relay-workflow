package ops

import (
	"context"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The reasons that close a node's execution (AgentClose): the node is given up and its slot goes back in the transaction that
// closes it. Every other reason of an escalation is a question for the orchestrator that leaves the node as it is.
var closingReasons = map[string]bool{"tiers_exhausted": true, "blocked_scope": true, "size_split": true}

// The reasons that put a node in a recovery hold: nothing is closed, the slot stays, and `crw flow recover` or `crw flow resume`
// takes the call.
const (
	holdDispatchUnknown = "dispatch_unknown"
	holdHostFailure     = "host_failure"
)

// ---- crw.escalate ----

// escalateOp is crw.escalate(node, info): the lane gives a node up, or asks the orchestrator what to do with it - a scope
// decision, a configuration fault, the end of the tiers, a reviewer that cannot judge. It writes the escalation row of the run
// (the row `crw flow show` and the shadow read) and, for a reason that gives the node up (tiers_exhausted, blocked_scope,
// size_split), asks the relay to close the node's execution, which returns its slot in the same transaction (R07). An escalation
// with a node of null is the milestone's.
type escalateOp struct{ l *Lane }

// EscalateInfo says why.
type EscalateInfo struct {
	Reason   string         `json:"reason"`
	Call     string         `json:"call"`
	Class    string         `json:"class"`
	Detail   string         `json:"detail"`
	Evidence map[string]any `json:"evidence"`
	// Ask is a question that waits for the orchestrator's decision (crw.decide).
	Ask string `json:"ask"`
}

// EscalateOutcome is the answer of crw.escalate.
type EscalateOutcome struct {
	Escalated    bool   `json:"escalated"`
	ID           string `json:"id"`
	Node         string `json:"node"`
	Reason       string `json:"reason"`
	Class        string `json:"class"`
	Closed       bool   `json:"closed"`
	SlotReleased bool   `json:"slot_released"`
}

func escalationClass(reason string) string {
	switch {
	case strings.HasPrefix(reason, "host_config_"), strings.HasPrefix(reason, "host_policy_"), reason == "pair_refused", reason == "policy_refused", reason == "schema_not_strict",
		reason == "independence_refused", reason == "independence_unproven", reason == "missing_step_decision", reason == "foreign_handle":
		return "configuration_fault"
	case reason == "blocked_scope", reason == "research_cap", reason == "size_split":
		return "scope"
	case reason == "tiers_exhausted":
		return "tiers"
	}
	return "decision"
}

func (o *escalateOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var info EscalateInfo
	if err := args(req.Args, &node, &info); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.escalate(node, info): %w", err)
	}
	if info.Reason == "" {
		return engine.OpResult{}, fmt.Errorf("crw.escalate: a reason is required")
	}
	if info.Class == "" {
		info.Class = escalationClass(info.Reason)
	}
	r := rows(req.Journal.Events())
	out := EscalateOutcome{Escalated: true, ID: "esc_" + hashParts(req.RunID, req.CallID)[:24], Node: node, Reason: info.Reason, Class: info.Class}
	// The row is written once for the op's call: an op that ran and did not write its own row runs again under the same call id.
	written := false
	for _, ev := range r.flow(engine.EvEscalation) {
		if dataStr(ev, "escalation_id") == out.ID {
			written = true
		}
	}
	if !written {
		data := map[string]any{"reason": info.Reason, "class": info.Class, "node": node, "call_id": info.Call, "escalation_id": out.ID, "detail": info.Detail, "evidence": info.Evidence, "ask": info.Ask}
		req.Journal.Append(engine.Event{RunID: req.RunID, Kind: engine.KindFlow, CallID: req.CallID, Op: OpEscalate, Event: engine.EvEscalation, Data: data})
	}
	if node != "" && closingReasons[info.Reason] {
		plan := ""
		if rel, ok := releaseRowOf(r, node); ok {
			plan = dataStr(rel, "plan")
		}
		res, err := o.l.d.Relay.Close(ctx, CloseRequest{Plan: plan, Node: node, Actor: o.l.d.Actor, Reason: info.Reason, RequestID: req.RequestID})
		if err != nil {
			return engine.OpResult{}, err
		}
		out.Closed, out.SlotReleased = res.Closed, res.SlotReleased
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{
		"node": node, "reason": info.Reason, "class": info.Class, "escalation_id": out.ID, "call": info.Call, "closed": out.Closed, "slot_released": out.SlotReleased, "request_id": req.RequestID,
	}}, nil
}

func (o *escalateOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.hold ----

// holdOp is crw.hold(node, info): the lane puts a node in a hold - a call of it held for the host (infrastructure, quota or budget)
// or ended with an outcome nobody knows. Nothing is closed and the slot stays; the relay is told only so that it can check the
// actor and the execution and answer that the node is held (AgentClose with a hold reason writes nothing). The node leaves the
// hold only through `crw flow resume` (a held call goes on in place) or `crw flow recover` (a call whose outcome is unknown).
type holdOp struct{ l *Lane }

// HoldInfo says why.
type HoldInfo struct {
	Reason string `json:"reason"`
	Call   string `json:"call"`
	Source string `json:"source"`
}

// HoldOutcome is the answer of crw.hold.
type HoldOutcome struct {
	Held   bool   `json:"held"`
	Node   string `json:"node"`
	Reason string `json:"reason"`
}

func (o *holdOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var info HoldInfo
	if err := args(req.Args, &node, &info); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.hold(node, info): %w", err)
	}
	if node == "" || info.Reason == "" {
		return engine.OpResult{}, fmt.Errorf("crw.hold: a node and a reason are required")
	}
	r := rows(req.Journal.Events())
	why := holdHostFailure
	if info.Reason == holdDispatchUnknown || info.Reason == engine.ReasonRecoveryHold {
		why = holdDispatchUnknown
	}
	if rel, ok := releaseRowOf(r, node); ok {
		if _, err := o.l.d.Relay.Close(ctx, CloseRequest{Plan: dataStr(rel, "plan"), Node: node, Actor: o.l.d.Actor, Reason: why, RequestID: req.RequestID}); err != nil {
			return engine.OpResult{}, err
		}
	}
	return engine.OpResult{Result: result(HoldOutcome{Held: true, Node: node, Reason: info.Reason}), Data: map[string]any{
		"node": node, "reason": info.Reason, "call": info.Call, "source": info.Source, "hold": why,
	}}, nil
}

func (o *holdOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.releaseChild ----

// releaseChildOp is crw.releaseChild(node, plan): a node the architect sized for a child (size_fit child, SP-CHILD-2) is released
// as an issue child - a managed start of an issue-lane run - instead of an executor. The child coordinates only: its executor
// calls author every commit.
type releaseChildOp struct{ l *Lane }

// ChildOutcome is the answer of crw.releaseChild.
type ChildOutcome struct {
	Started bool   `json:"started"`
	Node    string `json:"node"`
	Child   string `json:"child"`
	Reason  string `json:"reason"`
}

func (o *releaseChildOp) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var plan IssuePlan
	if err := args(req.Args, &node, &plan); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.releaseChild(node, plan): %w", err)
	}
	r := rows(req.Journal.Events())
	ms := plan.Milestone
	planID := planOf(r, ms)
	res, err := o.l.d.Relay.ReleaseChild(ctx, ChildRequest{Plan: planID, Node: node, RunID: req.RunID, Revision: plan.Revision, DesignDigest: plan.DesignDigest, Reason: plan.SizeReason, RequestID: req.RequestID})
	if err != nil {
		return engine.OpResult{}, err
	}
	out := ChildOutcome{Started: res.Started, Node: node, Child: res.Child, Reason: res.Reason}
	if res.Started {
		// the structural spawn of the child (SP-CHILD-1); the signal that sized the node for it (SP-CHILD-2) is the lane's own decision
		info := DecisionInfo{Node: node, Role: "child", Reason: plan.SizeReason, Inputs: map[string]any{"child": res.Child, "revision": plan.Revision}}
		if err := o.l.d.Decisions.AppendSpawn(ctx, o.l.spawnRow(req, RuleChild1, DecisionSpawn, info, "child")); err != nil {
			return engine.OpResult{}, err
		}
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{
		"node": node, "plan": planID, "milestone": ms, "child": res.Child, "revision": plan.Revision, "design_digest": plan.DesignDigest, "size_reason": plan.SizeReason, "started": res.Started,
	}}, nil
}

func (o *releaseChildOp) Proven(ctx context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	return provenAlways(ctx, ev, row)
}

// ---- crw.decide ----

// decideOp is crw.decide(node, ask): the wait for a decision the orchestrator makes. It reads the run journal for the one row
// that resumes a wait, decision_applied, which `crw flow answer` (F12) appends after it checked the answer (the question and its
// revision, the node, the authority) and applied it. A decision that is only recorded - an answer in the decisions record that the
// run has not applied - resumes nothing: the lane is waiting until the applied row is there. A resume asks again for a wait it
// left open, and the op's row says applied only once the journal had the applied row.
type decideOp struct{ l *Lane }

// Ask names the wait: the id of the escalation it answers.
type Ask struct {
	ID string `json:"id"`
}

// DecideOutcome is the answer of crw.decide.
type DecideOutcome struct {
	Applied  bool           `json:"applied"`
	Node     string         `json:"node"`
	Ask      string         `json:"ask"`
	Decision map[string]any `json:"decision"`
}

// appliedRow is the decision_applied row that answers an ask of a node.
func appliedRow(r rows, node, ask string) (engine.Event, bool) {
	return r.flow(EvDecisionApplied).last(func(ev engine.Event) bool {
		return dataStr(ev, "ask") == ask && (dataStr(ev, "node") == node || dataStr(ev, "node") == "")
	})
}

func (o *decideOp) Run(_ context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var node string
	var ask Ask
	if err := args(req.Args, &node, &ask); err != nil {
		return engine.OpResult{}, fmt.Errorf("crw.decide(node, ask): %w", err)
	}
	out := DecideOutcome{Node: node, Ask: ask.ID, Decision: map[string]any{}}
	if row, ok := appliedRow(rows(req.Journal.Events()), node, ask.ID); ok {
		out.Applied = true
		if d, ok := row.Data["decision"].(map[string]any); ok {
			out.Decision = d
		}
	}
	return engine.OpResult{Result: result(out), Data: map[string]any{"node": node, "ask": ask.ID, "applied": out.Applied}}, nil
}

func (o *decideOp) Proven(_ context.Context, ev []engine.Event, row engine.Event) (bool, string, error) {
	if dataBool(row, "applied") {
		return true, "", nil
	}
	// a wait that was open when the row was written is open still only while no applied row answers it
	if _, ok := appliedRow(rows(ev), dataStr(row, "node"), dataStr(row, "ask")); ok {
		return false, "the decision has been applied since", nil
	}
	return true, "", nil
}
