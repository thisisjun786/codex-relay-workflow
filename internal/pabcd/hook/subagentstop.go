package hook

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/projectcfg"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// SubagentStopPayload is the typed ingress supplied by harness without an import
// back to it. Only these fields participate in the evidence gate.
type SubagentStopPayload struct {
	Cwd, SessionID, AgentType, AgentID, TurnID, LastAssistantMessage string
	// AgentTranscriptPath is the child's own transcript, which starts with the packet it was dispatched with (CRW-1115).
	AgentTranscriptPath string
}

// RunSubagentStopGate ports CXC v0.2.40 subagent-evidence.ts:476-532 (3c1459ac).
// A receipt resolves before the terminal latch; three blocks spend the budget,
// then a durable negative verdict lets the child exit without waiving verification.
// The evidence assignments of CRW-1115 (a receipt in the assigned tree, a scope
// conflict) and the counter rules of CRW-1106 (one reader, a tuple lock) are
// recorded deviations: docs/port-cxc/known-defects/CRW-1115.md and CRW-1106.md.
func RunSubagentStopGate(p SubagentStopPayload, env func(string) string) (out string) {
	item := evidence.Payload{AgentType: p.AgentType, AgentID: p.AgentID, TurnID: p.TurnID, LastAssistantMessage: p.LastAssistantMessage}
	defer func() {
		if recover() != nil {
			out = ""
			subagentStopRecordInternalError(p, item)
		}
	}()
	if !projectcfg.PabcdEnabled(p.Cwd, env) || !evidence.IsGatedAgentType(p.AgentType) {
		return ""
	}
	if p.AgentType == "worker" {
		s, unreadable := state.ReadStateStrict(p.Cwd, p.SessionID)
		if unreadable || !s.OrchestrationActive || (s.Phase != state.PhaseB && s.Phase != state.PhaseC) {
			return ""
		}
	}
	// CRW-1106 (port: fixed): the whole decision runs under the lock of the exact (session, agent, turn), so stops of one child
	// that arrive together reserve the budget one at a time. A lock that cannot be had leaves the counter alone: the child is
	// released and its negative verdict recorded, as for any counter that cannot be advanced.
	decided := false
	if err := evidence.WithCounterLock(p.Cwd, p.SessionID, p.AgentID, p.TurnID, func() error {
		out, decided = subagentStopDecide(p, item, true), true
		return nil
	}); err != nil || !decided {
		return subagentStopDecide(p, item, false)
	}
	return out
}

// subagentStopDecide is the gate's decision for one stop. reserve says whether the caller holds the tuple's counter lock: without it
// the counter is never written, and a stop that would have spent an attempt is a terminal verdict instead.
func subagentStopDecide(p SubagentStopPayload, item evidence.Payload, reserve bool) string {
	// CRW-1115 (port: fixed): a child dispatched with an evidence assignment (found from its own transcript, see evidence.JudgeAssignedReceipt)
	// is judged by that contract alone, and a native-cwd receipt is no substitute for it. Only a child with no contract keeps the
	// native root, as in the oracle.
	if receipt, ok := evidence.ExtractReceiptPath(p.LastAssistantMessage); ok {
		verdict := evidence.JudgeAssignedReceipt(p.Cwd, p.SessionID, p.AgentID, p.AgentTranscriptPath, p.LastAssistantMessage, receipt)
		if verdict == evidence.AssignedAccepted || verdict == evidence.NoContract && evidence.HasValidReceipt(p.Cwd, receipt) {
			evidence.ClearAttempts(p.Cwd, p.SessionID, p.AgentID, p.TurnID)
			evidence.ResolveTombstone(p.Cwd, p.SessionID, item)
			return ""
		}
	}
	counter := evidence.ReadCounter(p.Cwd, p.SessionID, p.AgentID, p.TurnID)
	terminal := func(attempts int) string {
		evidence.RecordTombstone(p.Cwd, p.SessionID, item, attempts, evidence.WriteUnrecordableMarker)
		return ""
	}
	// A packet that allows no evidence write is released at once with an unverified verdict the parent must resolve with its own
	// verification; it is never a pass, and only the assignment the spawn hook recorded for it qualifies.
	if id, ok := evidence.ExtractScopeConflict(p.LastAssistantMessage); ok && evidence.ClaimScopeConflict(p.Cwd, p.SessionID, p.AgentID, p.AgentTranscriptPath, p.TurnID, id) {
		return terminal(counter.Attempts)
	}
	if evidence.HasTombstone(p.Cwd, p.SessionID, item) {
		return ""
	}
	// CRW-1106 (port: fixed): a counter that is exhausted, corrupt or unreadable ends the budget alike, and its bytes stay, so the
	// goal-complete gate, which reads the same snapshot, keeps refusing until a valid receipt clears both. The oracle read a
	// corrupt counter as 0 and wrote a fresh one over it.
	if counter.Spent() {
		return terminal(evidence.MaxAttempts)
	}
	next := counter.Attempts + 1
	if !reserve || !evidence.WriteAttempts(p.Cwd, p.SessionID, p.AgentID, next, p.TurnID) {
		return terminal(counter.Attempts)
	}
	return `{"decision":"block","reason":` + pyjson.Dumps(evidence.VerifierDirective(next), pyjson.Options{Unicode: true}) + `}`
}

func subagentStopRecordInternalError(p SubagentStopPayload, item evidence.Payload) {
	defer func() { _ = recover() }() // Recording failure must not prevent the child's exit.
	evidence.RecordTombstone(p.Cwd, p.SessionID, item, evidence.MaxAttempts, evidence.WriteUnrecordableMarker)
}
