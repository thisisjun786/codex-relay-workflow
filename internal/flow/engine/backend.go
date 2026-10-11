package engine

import (
	"context"
	"fmt"
)

// CallResult statuses.
const (
	StatusCompleted  = "completed"
	StatusNullModel  = "null_model"
	StatusHeld       = "held"
	StatusRefused    = "refused"
	StatusUnknown    = "unknown"
	StatusSuperseded = "superseded"
)

// The closed set of CallResult reasons that do not carry a host kind. host_infra_<kind>, host_config_<kind> and
// host_policy_<kind> complete the set (ClassifyHostFailure).
const (
	ReasonSchemaInvalidAfterRepair = "schema_invalid_after_repair"
	ReasonContextWindowExceeded    = "context_window_exceeded"
	ReasonHostInterruptedExternal  = "host_interrupted_external"
	ReasonEnginePaused             = "engine_paused"
	ReasonBudgetExhausted          = "budget_exhausted"
	ReasonPairRefused              = "pair_refused"
	ReasonPolicyRefused            = "policy_refused"
	ReasonIndependenceRefused      = "independence_refused"
	ReasonMissingStepDecision      = "missing_step_decision"
	ReasonSchemaNotStrict          = "schema_not_strict"
	ReasonForeignHandle            = "foreign_handle"
	ReasonDispatchUnknown          = "dispatch_unknown"
	ReasonSupersededByAttempt      = "superseded_by_attempt"
	ReasonSupersededByResume       = "superseded_by_resume"
	ReasonCancelled                = "cancelled"
)

// ThreadSpec is what a thread is created with. Every engine thread is persistent.
type ThreadSpec struct {
	CallID string
	Role   string
	Pair   Pair
	// Cwd is the call's private working directory ("" lets the backend choose).
	Cwd string
}

// ThreadInfo is what the backend reads back about a created thread: its id and the pair it is configured with.
type ThreadInfo struct {
	ID   string
	Pair Pair
}

// TurnSpec is one turn of a call. Op is the turn operation `<call_id>/t<k>`; the first line of Input carries it as
// `crw-flow-op: <op>`.
type TurnSpec struct {
	CallID   string
	Op       string
	K        int
	ThreadID string
	Role     string
	Pair     Pair
	Input    string
	// Schema is the strict outputSchema (nil for a call without one).
	Schema []byte
	Cwd    string
	Repair bool
	// Started is called once the host has accepted the turn and named it; the packet counts as delivered from then on.
	Started func(turnID string)
}

// Tokens is a turn's token usage.
type Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Total  int64 `json:"total"`
}

// TurnOutcome is a terminal turn: its last agent message, or a failure.
type TurnOutcome struct {
	TurnID string
	// Text is the turn's last agent message.
	Text   string
	Tokens Tokens
	// Failed is set for a terminal failed or interrupted turn.
	Failed *TurnFailure
}

// TurnFailure is a failed or interrupted turn as the host reported it.
type TurnFailure struct {
	// Interrupted is a turn the host reports as interrupted (the engine's own interrupts are judged by the engine).
	Interrupted bool
	// CodexErrorInfo is the Turn.error.codexErrorInfo variant name, verbatim; empty when the turn carries none.
	CodexErrorInfo string
	Message        string
}

// RPCError is an explicit error answer to thread/start or turn/start. CodexErrorInfo is the variant its error data
// carries, empty when it carries none.
type RPCError struct {
	Message        string
	CodexErrorInfo string
}

func (e *RPCError) Error() string {
	if e.CodexErrorInfo != "" {
		return fmt.Sprintf("rpc error (%s): %s", e.CodexErrorInfo, e.Message)
	}
	return "rpc error: " + e.Message
}

// TransportError is a failure that is not an answer: a lost connection, a timeout, a missing response. It is never
// classified as held; the engine ends the call outcome_unknown (F05's reconcile decides it once it exists).
type TransportError struct{ Err error }

func (e *TransportError) Error() string { return "transport error: " + e.Err.Error() }
func (e *TransportError) Unwrap() error { return e.Err }

// Backend is the host an engine runs agent calls on. The fake backend implements it for F01; the App Server
// backend (F04) implements it against Codex.
type Backend interface {
	// StartThread creates a persistent thread and reads its configured pair back.
	StartThread(ctx context.Context, spec ThreadSpec) (ThreadInfo, error)
	// RunTurn starts one turn on a thread and waits for its terminal state. It returns an *RPCError for an
	// explicit error answer and a *TransportError (or any other error) for a failure that is not an answer.
	RunTurn(ctx context.Context, spec TurnSpec) (TurnOutcome, error)
}

// Disposition is the status and reason a host failure ends a call with.
type Disposition struct {
	Status         string
	Reason         string
	CodexErrorInfo string
}

// infraKinds are the codexErrorInfo variants that hold the call (host_infra_<kind>); a usage or rate limit also pauses
// the run.
var infraKinds = map[string]bool{
	"usageLimitExceeded": true, "rateLimitExceeded": true, "serverOverloaded": true, "internalServerError": true,
	"httpConnectionFailed": true, "responseStreamConnectionFailed": true, "responseStreamDisconnected": true,
	"responseTooManyFailedAttempts": true, "threadRollbackFailed": true, "other": true, "activeTurnNotSteerable": true,
}

// PausesRun reports whether a held reason pauses the whole run (quota and rate limits) and not only the node.
func PausesRun(reason string) bool {
	return reason == "host_infra_usageLimitExceeded" || reason == "host_infra_rateLimitExceeded"
}

// ClassifyHostFailure maps a failed turn's, or an RPC error's, codexErrorInfo to a status and reason
// (engine.agentPrimitive.hostFailureMap). Only explicit answers are classified: a transport error never reaches
// here. A failure that carries no codexErrorInfo, or one the table does not list, is held host_infra_unknown, never
// null_model and never refused.
func ClassifyHostFailure(codexErrorInfo string) Disposition {
	d := Disposition{CodexErrorInfo: codexErrorInfo}
	switch {
	case infraKinds[codexErrorInfo]:
		d.Status, d.Reason = StatusHeld, "host_infra_"+codexErrorInfo
	case codexErrorInfo == "sessionBudgetExceeded":
		d.Status, d.Reason = StatusHeld, ReasonBudgetExhausted
	case codexErrorInfo == "badRequest" || codexErrorInfo == "unauthorized" || codexErrorInfo == "sandboxError":
		d.Status, d.Reason = StatusRefused, "host_config_"+codexErrorInfo
	case codexErrorInfo == "cyberPolicy" || codexErrorInfo == "misalignmentPolicyViolation":
		d.Status, d.Reason = StatusRefused, "host_policy_"+codexErrorInfo
	case codexErrorInfo == "contextWindowExceeded":
		d.Status, d.Reason = StatusNullModel, ReasonContextWindowExceeded
	default:
		d.Status, d.Reason = StatusHeld, "host_infra_unknown"
	}
	return d
}
