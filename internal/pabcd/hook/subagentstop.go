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
}

// RunSubagentStopGate ports CXC v0.2.40 subagent-evidence.ts:476-532 (3c1459ac).
// A receipt resolves before the terminal latch; three blocks spend the budget,
// then a durable negative verdict lets the child exit without waiving verification.
// The evidence assignments of CRW-1115 (a receipt in the assigned tree, a scope
// conflict) are a recorded deviation: docs/port-cxc/known-defects/CRW-1115.md.
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
	// CRW-1115 (port: fixed): besides the native root, a receipt in the tree the parent's packet assigned this child counts, bound
	// to the first child that claims it (evidence.AcceptAssignedReceipt). Without an assignment the native root is the only one.
	if receipt, ok := evidence.ExtractReceiptPath(p.LastAssistantMessage); ok &&
		(evidence.HasValidReceipt(p.Cwd, receipt) || evidence.AcceptAssignedReceipt(p.Cwd, p.SessionID, p.AgentID, receipt)) {
		evidence.ClearAttempts(p.Cwd, p.SessionID, p.AgentID, p.TurnID)
		evidence.ResolveTombstone(p.Cwd, p.SessionID, item)
		return ""
	}
	// A packet that allows no evidence write is released at once with an unverified verdict the parent must resolve with its own
	// verification; it is never a pass, and only the assignment the spawn hook recorded for it qualifies.
	if id, ok := evidence.ExtractScopeConflict(p.LastAssistantMessage); ok && evidence.ClaimScopeConflict(p.Cwd, p.SessionID, p.AgentID, p.TurnID, id) {
		evidence.RecordTombstone(p.Cwd, p.SessionID, item, evidence.ReadAttempts(p.Cwd, p.SessionID, p.AgentID, p.TurnID), evidence.WriteUnrecordableMarker)
		return ""
	}
	if evidence.HasTombstone(p.Cwd, p.SessionID, item) {
		return ""
	}
	attempts := evidence.ReadAttempts(p.Cwd, p.SessionID, p.AgentID, p.TurnID)
	if attempts >= evidence.MaxAttempts {
		evidence.RecordTombstone(p.Cwd, p.SessionID, item, attempts, evidence.WriteUnrecordableMarker)
		return ""
	}
	next := attempts + 1
	if !evidence.WriteAttempts(p.Cwd, p.SessionID, p.AgentID, next, p.TurnID) {
		evidence.RecordTombstone(p.Cwd, p.SessionID, item, attempts, evidence.WriteUnrecordableMarker)
		return ""
	}
	return `{"decision":"block","reason":` + pyjson.Dumps(evidence.VerifierDirective(next), pyjson.Options{Unicode: true}) + `}`
}

func subagentStopRecordInternalError(p SubagentStopPayload, item evidence.Payload) {
	defer func() { _ = recover() }() // Recording failure must not prevent the child's exit.
	evidence.RecordTombstone(p.Cwd, p.SessionID, item, evidence.MaxAttempts, evidence.WriteUnrecordableMarker)
}
