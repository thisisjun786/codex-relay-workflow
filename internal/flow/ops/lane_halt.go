package ops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The reasons that close a node's execution (AgentClose): the node is given up and its slot goes back in the transaction that
// closes it. Every other reason of an escalation is a question for the orchestrator that leaves the node as it is.
var closingReasons = map[string]bool{"tiers_exhausted": true, "blocked_scope": true, "size_split": true}

// ReasonLandEnded is the reason the relay closes the execution of a land run's node with when the land ends without landing (an
// escalation with end_land, CRW-1354): the run gives its release back - the closure is cancelled and the slot returns in its transaction -
// so the child's next land of the node is a new release of its own run.
const ReasonLandEnded = "land_ended"

// closeRetryWaits are the waits before each new try of the close of an ended land after a transient relay error (CRW-1397).
var closeRetryWaits = []time.Duration{100 * time.Millisecond, 400 * time.Millisecond}

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
	// EndLand: the escalation ends a gate-only land run (land-head) whose head did not pass its gates, and the run gives the node's
	// release back (AgentClose land_ended), whatever the reason. A close the relay refuses because the execution has to stay (an
	// accepted head that landed or whose integration may have moved the target, ErrCloseConflict) is recorded on the row and ends nothing
	// else; any other relay error is asked again, and is the op's error when it keeps failing (CRW-1397); a lost epoch is the run's.
	EndLand bool `json:"end_land"`
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
	closeRefused := ""
	if node != "" && (closingReasons[info.Reason] || info.EndLand) {
		plan := ""
		if rel, ok := releaseRowOf(r, node); ok {
			plan = dataStr(rel, "plan")
		}
		reason := info.Reason
		if !closingReasons[reason] {
			reason = ReasonLandEnded
		}
		closeExecution := func() (CloseResult, error) {
			return o.l.d.Relay.Close(ctx, CloseRequest{Plan: plan, Node: node, Actor: o.l.d.Actor, Reason: reason, RequestID: req.RequestID})
		}
		res, err := closeExecution()
		// a gate-only land asks again after a relay error that is neither the relay keeping the execution nor a lost epoch (CRW-1397):
		// the close is the relay's replay, so asking again is safe, and a land must not end with its execution current under a run
		// that has ended. One that keeps failing is the op's error: the run ends on it and its resume asks again.
		for i := 0; err != nil && info.EndLand && !errors.Is(err, ErrCloseConflict) && !errors.Is(err, ErrEpochLost) && i < len(closeRetryWaits); i++ {
			select {
			case <-ctx.Done():
				return engine.OpResult{}, errors.Join(err, ctx.Err())
			case <-time.After(closeRetryWaits[i]):
			}
			res, err = closeExecution()
		}
		switch {
		case err != nil && info.EndLand && errors.Is(err, ErrCloseConflict):
			// the relay keeps the execution: its accepted head landed or may be in the target
			closeRefused = err.Error()
		case err != nil:
			return engine.OpResult{}, err
		default:
			out.Closed, out.SlotReleased = res.Closed, res.SlotReleased
		}
	}
	data := map[string]any{
		"node": node, "reason": info.Reason, "class": info.Class, "escalation_id": out.ID, "call": info.Call, "closed": out.Closed, "slot_released": out.SlotReleased, "request_id": req.RequestID,
	}
	if info.EndLand {
		data["end_land"] = true
	}
	if closeRefused != "" {
		// warn + record: the land ends escalated with its execution as the relay keeps it
		data["close_refused"] = closeRefused
	}
	return engine.OpResult{Result: result(out), Data: data}, nil
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
