package engine

import (
	"strings"
	"sync"
)

// Event kinds.
const (
	KindRun  = "run"  // the run record
	KindCall = "call" // one event of a call (engine.dispatch.events)
	KindFlow = "flow" // phase, log, signal rows
)

// Call events. prepared .. turn_started are intents and progress; the last six are terminal.
const (
	EvPrepared        = "prepared"
	EvThreadRequested = "thread_requested"
	EvThreadStarted   = "thread_started"
	EvForkRequested   = "fork_requested"
	EvForkStarted     = "fork_started"
	EvTurnRequested   = "turn_requested"
	EvTurnStarted     = "turn_started"
	// EvInterruptRequested is the engine's intent to interrupt the turn in flight (the pause of a run); it is written
	// before the interrupt is sent, and decides how the interrupted turn is classified.
	EvInterruptRequested = "interrupt_requested"

	EvCompleted      = "completed"
	EvNullModel      = "null_model"
	EvHeld           = "held"
	EvRefused        = "refused"
	EvOutcomeUnknown = "outcome_unknown"
	EvSuperseded     = "superseded"
)

// terminalEvents are the events that end a call; the CallResult status is that of the last one.
var terminalEvents = map[string]string{
	EvCompleted:      StatusCompleted,
	EvNullModel:      StatusNullModel,
	EvHeld:           StatusHeld,
	EvRefused:        StatusRefused,
	EvOutcomeUnknown: StatusUnknown,
	EvSuperseded:     StatusSuperseded,
}

// Event is one append-only journal row. The in-memory journal stands in for the zone tables (flow_runs, flow_calls,
// flow_events) that arrive with the store (F06); a row is never updated.
type Event struct {
	Seq      int            `json:"seq"`
	RunID    string         `json:"runId"`
	Kind     string         `json:"kind"`
	CallID   string         `json:"callId,omitempty"`
	Path     string         `json:"path,omitempty"`
	Position int            `json:"position,omitempty"`
	Op       string         `json:"op,omitempty"` // agent, crw.clock, join, workflow: what occupies the position
	Label    string         `json:"label,omitempty"`
	Event    string         `json:"event"`
	Data     map[string]any `json:"data,omitempty"`
}

// Journal is what the engine appends to. Implementations must be safe for concurrent use.
type Journal interface {
	// Append stores e, assigning its Seq, and returns it.
	Append(e Event) Event
	// Events returns every row in append order.
	Events() []Event
}

// MemoryJournal is the in-memory journal.
type MemoryJournal struct {
	mu     sync.Mutex
	events []Event
}

// NewMemoryJournal returns an empty journal.
func NewMemoryJournal() *MemoryJournal { return &MemoryJournal{} }

// Append implements Journal.
func (j *MemoryJournal) Append(e Event) Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	e.Seq = len(j.events) + 1
	j.events = append(j.events, e)
	return e
}

// Events implements Journal.
func (j *MemoryJournal) Events() []Event {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]Event(nil), j.events...)
}

// CallEvents returns the rows of one call in append order.
func CallEvents(j Journal, callID string) []Event {
	var out []Event
	for _, e := range j.Events() {
		if e.Kind == KindCall && e.CallID == callID {
			out = append(out, e)
		}
	}
	return out
}

// CallInfo is what the journal says about one call: the record a handle resolves through and a continuation
// inherits from.
type CallInfo struct {
	RunID, CallID, Path string
	Role, Family        string
	Node, StepDecision  string
	Pair                Pair
	ThreadID            string
	// Configured is the pair the host read back for the thread (carried unchanged along a chain of continuations); a
	// thread whose readback differs from its role's pair is never continued.
	Configured Pair
	// Cwd is the working directory of the call's thread, as the host read it back when the thread started (or was forked).
	Cwd string
	// ThroughTurnID is the last turn of the call: a continuation resumes the thread after it.
	ThroughTurnID string
	Terminal      string // the status of the call's last terminal event, empty while it is open
}

// LookupCall reads a call's record from the journal's own rows.
func LookupCall(j Journal, callID string) (CallInfo, bool) {
	return lookupCall(j.Events(), callID)
}

// lookupCall reads a call's record from rows, which may hold the rows of other calls.
func lookupCall(events []Event, callID string) (CallInfo, bool) {
	var info CallInfo
	found := false
	for _, e := range events {
		if e.Kind != KindCall || e.CallID != callID {
			continue
		}
		found = true
		d := e.Data
		switch e.Event {
		case EvPrepared:
			info.RunID, info.CallID, info.Path = e.RunID, e.CallID, e.Path
			info.Role, _ = d["role"].(string)
			info.Family, _ = d["family"].(string)
			info.Node, _ = d["node"].(string)
			info.StepDecision, _ = d["step_decision"].(string)
			info.Pair.Model, _ = d["model"].(string)
			info.Pair.Effort, _ = d["effort"].(string)
			if t, _ := d["thread_id"].(string); t != "" {
				info.ThreadID = t
			}
			if t, _ := d["through_turn_id"].(string); t != "" {
				info.ThroughTurnID = t
			}
			info.Configured.Model, _ = d["configured_model"].(string)
			info.Configured.Effort, _ = d["configured_effort"].(string)
			if cwd, _ := d["thread_cwd"].(string); cwd != "" {
				info.Cwd = cwd
			}
		case EvThreadStarted, EvForkStarted:
			info.ThreadID, _ = d["thread_id"].(string)
			info.Configured.Model, _ = d["model"].(string)
			info.Configured.Effort, _ = d["effort"].(string)
			info.Cwd, _ = d["cwd"].(string)
		case EvTurnStarted:
			info.ThroughTurnID, _ = d["turn_id"].(string)
		}
		if st, ok := terminalEvents[e.Event]; ok {
			info.Terminal = st
		} else {
			// A row that is not terminal after a terminal one (reconcile's recovered rows after an unknown outcome) reopens
			// the call.
			info.Terminal = ""
		}
	}
	return info, found
}

// Handle is the opaque continuation token of a call: crwh1:<run>:<call_id>. It resolves through the journal to the
// call's thread and last turn.
func Handle(runID, callID string) string { return "crwh1:" + runID + ":" + callID }

// ParseHandle splits a handle into its run and call ids.
func ParseHandle(h string) (runID, callID string, ok bool) {
	parts := strings.SplitN(h, ":", 3)
	if len(parts) != 3 || parts[0] != "crwh1" || parts[1] == "" || parts[2] == "" {
		return "", "", false
	}
	return parts[1], parts[2], true
}
