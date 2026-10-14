// Package callstate says what a flow call's rows leave it as: settled, in a recovery hold, or still to be resumed. It is the one
// reading of a call's rows that `crw flow recover` (which accepts only a call in recovery hold), `crw flow resume` (which goes
// on with every other unsettled call) and `dag-restart` (which tells the orchestrator which of the two to run) share, so that
// the command a report names is the command that accepts the call. It names events by their journal spelling and imports
// nothing, so the relay's scheduler and the flow engine can both use it.
package callstate

// The journal events the reading depends on (internal/flow/engine names the same strings; a test pins them to each other).
const (
	Prepared        = "prepared"
	ThreadRequested = "thread_requested"
	ThreadStarted   = "thread_started"
	ForkRequested   = "fork_requested"
	ForkStarted     = "fork_started"
	TurnRequested   = "turn_requested"
	TurnStarted     = "turn_started"
	Interrupt       = "interrupt_requested"
	// StepPublished and SessionAttemptLost are the rows of a session call (CRW-1316): a step it published for the owner thread,
	// and an attempt whose subagent was lost.
	StepPublished      = "step_published"
	SessionAttemptLost = "session_attempt_lost"
	Completed          = "completed"
	NullModel          = "null_model"
	Held               = "held"
	Refused            = "refused"
	OutcomeUnknown     = "outcome_unknown"
	Superseded         = "superseded"
	RecoveryCleared    = "recovery_cleared"
)

// DispatchUnknown is the reason of an outcome that nobody could prove.
const DispatchUnknown = "dispatch_unknown"

// Row is one row of a call: its event and the reason it carries ("" when it carries none).
type Row struct {
	Event, Reason string
}

// Class is what a call's rows say.
type Class string

const (
	// Settled is a call whose last row ended it for good: completed, null_model, refused or superseded.
	Settled Class = "settled"
	// RecoveryHold is a call whose outcome was left unknown (outcome_unknown, reason dispatch_unknown, at a dispatch intent)
	// and has not been cleared since: the only manual way out is `crw flow recover`, and the node takes nothing until then.
	RecoveryHold Class = "recovery_hold"
	// Unsettled is every other call that has not ended: a dispatch intent nobody answered, a call whose hold was cleared, a
	// call that ended held (an engine pause, a budget stop, a host failure), an unknown outcome of another kind. A resume of
	// the run goes on with it.
	Unsettled Class = "unsettled"
)

func isTerminal(event string) bool {
	switch event {
	case Completed, NullModel, Held, Refused, OutcomeUnknown, Superseded:
		return true
	}
	return false
}

// Classify reads a call's rows in the order they were written. A prepared row starts an attempt; an intent or a started
// thread or turn (a session call's step_published and session_attempt_lost among them), and a recovery_cleared, make a call
// that ended open again; the last terminal row decides the rest.
func Classify(rows []Row) Class {
	var last, terminal, unknownReason string
	for _, r := range rows {
		switch r.Event {
		case Prepared:
			last, terminal, unknownReason = Prepared, "", ""
		case ThreadRequested, ForkRequested, ThreadStarted, ForkStarted, TurnRequested, TurnStarted, StepPublished, SessionAttemptLost:
			last, terminal = r.Event, ""
		case Interrupt:
		case RecoveryCleared:
			terminal = ""
		default:
			if isTerminal(r.Event) {
				terminal = r.Event
				if r.Event == OutcomeUnknown {
					unknownReason = r.Reason
				}
			}
		}
	}
	switch terminal {
	case Completed, NullModel, Refused, Superseded:
		return Settled
	case OutcomeUnknown:
		switch last {
		case ThreadRequested, ForkRequested, TurnRequested, TurnStarted:
			if unknownReason == DispatchUnknown {
				return RecoveryHold
			}
		}
	case "":
		if last == "" {
			return Settled // no row of an attempt: nothing was started
		}
	}
	return Unsettled
}
