package engine

import (
	"context"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
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
	// ReasonWorktreeFailed ends a call whose worktree could not be decided (refused: no start commit is recorded) or
	// made (held: git failed; nothing was sent to the host), or a completed executor turn whose worktree could not be
	// snapshotted (held: the pending tree P cannot be recorded; the turn was sent and its row carries pending_tree_error).
	ReasonWorktreeFailed = "worktree_failed"
	// ReasonRestoreSnapshotMismatch is the reason of a call that adopted a restored worktree whose live snapshot is not
	// the one the restore verified: it sent nothing further and its node stops with an escalation. Every later call of
	// the node in the run ends held with this reason, sending nothing.
	ReasonRestoreSnapshotMismatch = "restore_snapshot_mismatch"
	// ReasonAcceptedNodeChanged refuses a call of a node that was accepted when its spec or packet changed on a resume: the
	// refusal names the route (a plan revision with update_node, or dag-correct for an oversized child).
	ReasonAcceptedNodeChanged = "accepted_node_changed"
	// ReasonRecoveryHold ends a call whose node holds a call with an unknown outcome (recovery_hold): nothing was sent.
	ReasonRecoveryHold = "recovery_hold"
)

// ThreadSpec is what a thread is created with. Every engine thread is persistent.
type ThreadSpec struct {
	CallID string
	Role   string
	Pair   Pair
	// Sandbox is the sandbox of the role's thread (read-only or workspace-write).
	Sandbox string
	// Cwd is the call's private working directory ("" lets the backend choose).
	Cwd string
	// Node is the node the call belongs to ("" for a call without one).
	Node string
	// Head is the commit a reviewer reads (the head option), "" for any other call.
	Head string
	// Placement is where the call runs when the backend placed it (Placer); Cwd is then its Path.
	Placement Placement
	// AutoCompactTokenLimit is the role's model_auto_compact_token_limit, nil when its pair declares none.
	AutoCompactTokenLimit *int64
	// Send is the send ordinal of the creation request: 1 for the first, one more for each time reconcile proved the
	// previous one absent and sent it again.
	Send int
	// QA is set on the thread of a QA capture call: the backend gives it the QA overlay and no native helper.
	QA bool
}

// ThreadInfo is what the backend reads back about a created thread: its id and the pair it is configured with.
type ThreadInfo struct {
	ID   string
	Pair Pair
	// Cwd is the working directory the host reports for the thread ("" when the backend does not read it).
	Cwd string
}

// TurnSpec is one turn of a call. Op is the turn operation `<call_id>/t<k>`; the first line of Input carries it as
// `crw-flow-op: <op>`.
type TurnSpec struct {
	CallID   string
	Op       string
	K        int
	ThreadID string
	Role     string
	Node     string
	Pair     Pair
	Input    string
	// Schema is the strict outputSchema (nil for a call without one).
	Schema []byte
	// Cwd is the working directory of the thread the turn runs on ("" when the journal does not name one). It is the
	// call's own: a backend decides whether a turn of the role may carry it (only an architect turn does).
	Cwd    string
	Repair bool
	// QA is set on a turn of a QA capture call's thread (the capture turn and the turns that continue it).
	QA bool
	// Started is called once the host has accepted the turn and named it; the packet counts as delivered from then on.
	Started func(turnID string)
}

// Tokens is a turn's token usage.
type Tokens struct {
	Input  int64 `json:"input"`
	Output int64 `json:"output"`
	Total  int64 `json:"total"`
}

// Add is the sum of two usages.
func (t Tokens) Add(o Tokens) Tokens {
	return Tokens{Input: t.Input + o.Input, Output: t.Output + o.Output, Total: t.Total + o.Total}
}

// TurnOutcome is a terminal turn: its last agent message, or a failure.
type TurnOutcome struct {
	TurnID string
	// Text is the turn's last agent message.
	Text   string
	Tokens Tokens
	// TokensUnmeasured is set on a completed turn whose token usage the backend never received (a lost notification,
	// a turn recovered by a read): Tokens is then not a measurement, and zero is not what the turn used.
	TokensUnmeasured bool
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

// CodexErrorFlowBinding is the CodexErrorInfo a backend gives the *RPCError of a turn it did not send because the flow binding of
// its thread (F14) could not be written. It is not a variant the host knows: the call is held, host_config_flow_binding, with
// nothing sent, and the operator repairs the flow-bound directory (it is the current user's, mode 0700, no symlink) and resumes.
const CodexErrorFlowBinding = "flow_binding"

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
	case codexErrorInfo == CodexErrorFlowBinding:
		d.Status, d.Reason = StatusHeld, "host_config_"+codexErrorInfo
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

// Placement kinds.
const (
	// PlaceFresh is a new attempt worktree an executor call that creates a thread runs in.
	PlaceFresh = "fresh"
	// PlaceRestore is the worktree a resume suffix restored, adopted unchanged by the node's first executor call.
	PlaceRestore = "restore"
	// PlaceCall is the call-private working directory of any other role.
	PlaceCall = "call"
	// PlaceContinued is a continuation: the thread keeps the directory it was started in and nothing is made.
	PlaceContinued = "continued"
)

// Placement is where a call's thread runs and how that directory came to be. The engine journals it in the call's
// prepared row (key allocation) before anything exists.
type Placement struct {
	Kind    string `json:"kind"`
	Attempt int    `json:"attempt,omitempty"`
	Lineage int    `json:"lineage,omitempty"`
	Path    string `json:"path,omitempty"`
	// Start is the commit the directory starts at (C).
	Start string `json:"start_commit,omitempty"`
	// Pending is the restored pending tree (P) of a restore, empty when the prefix ended without one.
	Pending string `json:"pending,omitempty"`
	// Digest is the verified working-snapshot digest of a restore.
	Digest string `json:"snapshot_digest,omitempty"`
	// Fork is set on a continuation that runs in a restored worktree: it forks its handle's thread into it.
	Fork bool `json:"fork,omitempty"`
}

// PlaceRequest is what a placer is asked about.
type PlaceRequest struct {
	RunID, CallID, Role, Family, Node, Head string
	// QA is the QA capture call: an executor call that gets a call-private scratch worktree at Head, as the roles that do not
	// write get one, and no node worktree.
	QA bool
	// Continues is the record of the handle a continuation resumes, nil for a call that creates a thread.
	Continues *CallInfo
	// Journal is the run's journal: the allocations of the node so far are read from it.
	Journal Journal
	// Fork is set on a continuation inside a resume's suffix, whose handle's thread belongs to the previous process: it runs on
	// a fork of that thread in a directory of its own (the restored worktree for an executor, the call's private directory
	// for any other role), never on the thread whose cwd is a worktree that was retired.
	Fork bool
}

// SnapshotMismatch is the error Establish and Recheck return when the live working snapshot of a restored worktree is
// not the one its restore verified.
type SnapshotMismatch struct{ Want, Got string }

func (e *SnapshotMismatch) Error() string {
	return fmt.Sprintf("the restored worktree changed: snapshot %s, verified %s", e.Got, e.Want)
}

// Placer is implemented by a backend that decides where a call runs and makes that place. The engine asks Place while it
// writes the call's prepared row, calls Establish after that row and before thread_requested (or fork_requested), and
// Recheck before the first turn_requested of a restore; the worktree exists only after the row that names it.
type Placer interface {
	// Place decides the call's directory. It reads the journal and the repository and writes nothing.
	Place(req PlaceRequest) (Placement, error)
	// Establish makes the placement real: it creates the worktree, or for a restore compares the live snapshot with the
	// verified digest and returns a *SnapshotMismatch when they differ. Any other error is a worktree failure.
	Establish(ctx context.Context, req PlaceRequest, p Placement) error
	// Recheck compares the live snapshot of a restore with its verified digest again.
	Recheck(ctx context.Context, p Placement) error
	// Snapshot is the working snapshot of a worktree (the P_held of a held executor call).
	Snapshot(ctx context.Context, path string) (string, error)
}

// DirRemover is implemented by a backend that can remove the directory it made for a call once the call has its terminal row. The
// engine asks it for the directory of a continuation that runs at a head of its own (an architect turn that names `head`): the
// directory is read while the turn runs, a resume that runs the same call again makes it again at the head the prepared row
// records, and the path of a call is unique, so no other call's cwd overlaps it.
type DirRemover interface {
	RemoveDir(ctx context.Context, path string) error
}

// SourceReader is implemented by a backend that can say which source tree the directory of a call holds. The engine asks it when a
// QA call (the capture call of a final gate, or a turn that continues it) completes: that call may edit its scratch worktree, so the
// head it was started at is not what it exercised, and the identity of the tree it ended in is recorded on its completed row
// (qa_source). head is the commit the call names. A backend without it records nothing, and the QA evidence made on it is
// INCONCLUSIVE.
type SourceReader interface {
	SourceOf(ctx context.Context, cwd, head string) (source.Identity, error)
}

// ForkSpec is a thread/fork request of a continuation that runs in a restored worktree.
type ForkSpec struct {
	CallID, Role, Node string
	Pair               Pair
	Sandbox            string
	Cwd                string
	SourceThreadID     string
	ThroughTurnID      string
	// QA is set when the forked thread is a QA capture call's.
	QA bool
	// ThroughOp is the operation marker of the through turn (`<call_id>/t<k>`), "" when the journal names none: the fork's
	// history is checked to end at the turn by its id or by this marker.
	ThroughOp             string
	AutoCompactTokenLimit *int64
	// Send is the send ordinal of the creation request (see ThreadSpec).
	Send int
}

// Forker is implemented by a backend that can fork a thread at a turn into another directory.
type Forker interface {
	ForkThread(ctx context.Context, spec ForkSpec) (ThreadInfo, error)
}

// Reconciled is implemented by a backend whose requests that got no answer are decided afterwards by the reconcile pass
// (F05). The engine then writes no terminal row for such a call: its last row stays the intent that was sent, and the
// script sees status unknown. A backend without it (the fake) has the call end outcome_unknown.
type Reconciled interface {
	LeavesLostRequestsOpen() bool
}

// Map is the placement as the prepared row of a call records it.
func (p Placement) Map() map[string]any {
	m := map[string]any{"kind": p.Kind}
	if p.Attempt != 0 {
		m["attempt"] = p.Attempt
		m["lineage"] = p.Lineage
	}
	if p.Path != "" {
		m["path"] = p.Path
	}
	if p.Start != "" {
		m["start_commit"] = p.Start
	}
	if p.Pending != "" {
		m["pending"] = p.Pending
	}
	if p.Digest != "" {
		m["snapshot_digest"] = p.Digest
	}
	if p.Fork {
		m["fork"] = true
	}
	return m
}
