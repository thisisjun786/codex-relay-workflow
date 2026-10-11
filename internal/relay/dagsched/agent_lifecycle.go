package dagsched

import (
	"context"
	"errors"
)

// Stubs of the agent lifecycle (R07, CRW-1215): the signatures the tests are written against. The behaviour follows in the next commit.

const (
	AgentReasonFlowCancel         = "flow_cancel"
	AgentEscalationTiersExhausted = "tiers_exhausted"
	AgentEscalationBlockedScope   = "blocked_scope"
	AgentEscalationSizeSplit      = "size_split"
	AgentHoldDispatchUnknown      = "dispatch_unknown"
	AgentHoldHostFailure          = "host_failure"
)

const (
	AgentClosureCancelled  = "cancelled"
	AgentClosureEscalated  = "escalated"
	AgentClosureSuperseded = "superseded"
	AgentClosureRevoked    = "revoked"
)

// AgentCloseInput is what ends, or holds, the current execution of an agent node.
type AgentCloseInput struct{ Reason string }

// AgentCloseResult is what AgentClose answers.
type AgentCloseResult struct {
	PlanID, NodeID, ExecutionID, State, Reason, RevokedAcceptance string
	Closed, Held, SlotReleased, Replayed                          bool
}

func (s *Scheduler) AgentClose(ctx context.Context, plan, node, actor string, in AgentCloseInput) (AgentCloseResult, error) {
	return AgentCloseResult{}, errors.New("not implemented")
}

// AgentCorrectResult is what AgentCorrect answers.
type AgentCorrectResult struct {
	AgentReleaseResult
	SupersededAcceptance string
}

func (s *Scheduler) AgentCorrect(ctx context.Context, plan, node, actor string, req ReleaseRequest, opt AgentReleaseOptions) (AgentCorrectResult, error) {
	return AgentCorrectResult{}, errors.New("not implemented")
}

const (
	ActionAgentCorrection  = "agent_correction"
	AgentStateReleased     = "released"
	AgentStateAccepted     = "accepted"
	AgentStateIntegrated   = "integrated"
	AgentStateHeld         = "held"
	AgentActionRunResume   = "run resume"
	AgentActionFlowRecover = "crw flow recover"
	AgentActionAdopt       = "dag-adopt"
	AgentActionIntegrate   = "dag-integrate"
)

// RestartAgent is what dag-restart says of an agent node.
type RestartAgent struct {
	ExecutionID, ExecutionKind, State, Actor, RunID, Action string
	Attempt                                                 int64
}
