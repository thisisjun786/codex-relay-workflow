package engine

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/callstate"
)

// Source is the source of the threads the stub host's backend creates.
func (s *stubHost) Source() string { return "appServer" }

// heldAtTurnStarted is the journal of a call whose turn_started is recorded, whose outcome reconcile left unknown.
func heldAtTurnStarted() *MemoryJournal {
	j := NewMemoryJournal()
	add := func(ev string, data map[string]any) {
		j.Append(Event{RunID: "r", Kind: KindCall, CallID: "c1", Path: "root", Position: 1, Op: "agent", Label: "a", Event: ev, Data: data})
	}
	add(EvPrepared, map[string]any{"role": "executor", "node": "n1"})
	add(EvThreadRequested, map[string]any{"cwd": "/w/a1", "send": 1, "at": int64(1000), "owner": "p1"})
	add(EvThreadStarted, map[string]any{"thread_id": "t1", "model": "m", "effort": "e", "cwd": "/w/a1"})
	add(EvTurnRequested, map[string]any{"k": 1, "op": "c1/t1", "input": "go"})
	add(EvTurnStarted, map[string]any{"k": 1, "op": "c1/t1", "turn_id": "uX"})
	add(EvOutcomeUnknown, map[string]any{"reason": ReasonDispatchUnknown, "detail": "unknown"})
	return j
}

// A fork of a fork holds the turns its source inherited as well as the source's own: the prefix a fork request records is
// the whole history through the boundary, not only the turns the journal attributes to the source thread's calls.
func TestForkPrefixCarriesTheSourcesInheritedTurns(t *testing.T) {
	j := NewMemoryJournal()
	add := func(call, ev string, data map[string]any) {
		j.Append(Event{RunID: "r", Kind: KindCall, CallID: call, Path: "root", Op: "agent", Label: call, Event: ev, Data: data})
	}
	add("c0", EvPrepared, map[string]any{"node": "n1"})
	add("c0", EvThreadStarted, map[string]any{"thread_id": "T0"})
	add("c0", EvTurnStarted, map[string]any{"k": 1, "op": "c0/t1", "turn_id": "t0a"})
	add("c0", EvTurnStarted, map[string]any{"k": 2, "op": "c0/t2", "turn_id": "t0b"})
	// c1 forks T0 through t0b and adds a turn of its own on the fork T1
	add("c1", EvPrepared, map[string]any{"node": "n1", "thread_id": "T0", "through_turn_id": "t0b"})
	add("c1", EvForkRequested, map[string]any{"thread_id": "T0", "through_turn_id": "t0b", "prefix": threadPrefix(j, "T0", "t0b"), "cwd": "/w/1", "send": 1})
	add("c1", EvForkStarted, map[string]any{"thread_id": "T1", "cwd": "/w/1"})
	add("c1", EvTurnStarted, map[string]any{"k": 1, "op": "c1/t1", "turn_id": "t1a"})
	got := threadPrefix(j, "T1", "t1a")
	var ids []string
	for _, p := range got {
		ids = append(ids, p["turn_id"].(string))
	}
	if want := []string{"t0a", "t0b", "t1a"}; !slices.Equal(ids, want) {
		t.Fatalf("the prefix of T1 through t1a is %v, want the inherited turns and then its own %v", ids, want)
	}
	// a boundary inside the inherited part cuts there
	got = threadPrefix(j, "T1", "t0a")
	if len(got) != 1 || got[0]["turn_id"] != "t0a" {
		t.Errorf("the prefix of T1 through t0a is %v", got)
	}
	// a fork of the fork whose answer was lost is adopted: the host copied the inherited turns too
	add("c2", EvPrepared, map[string]any{"node": "n1", "thread_id": "T1", "through_turn_id": "t1a"})
	add("c2", EvForkRequested, map[string]any{"thread_id": "T1", "through_turn_id": "t1a", "prefix": threadPrefix(j, "T1", "t1a"), "cwd": "/w/2", "send": 1, "at": int64(1000), "owner": "p1"})
	host := &stubHost{
		threads: []HostThread{{ID: "T2", Cwd: "/w/2", Source: "appServer", ForkedFromID: "T1", CreatedAt: time.UnixMilli(1000), Status: "idle"}},
		turns: map[string][]HostTurn{"T2": {
			{ID: "t0a", Status: "completed", Readable: true, Marker: "c0/t1"},
			{ID: "t0b", Status: "completed", Readable: true, Marker: "c0/t2"},
			{ID: "t1a", Status: "completed", Readable: true, Marker: "c1/t1"}}},
	}
	rep, err := Reconcile(context.Background(), j, ReconcileConfig{RunID: "r", Host: host})
	if err != nil {
		t.Fatal(err)
	}
	if rec := rep.ByCall["c2"]; rec == nil || rec.Action != RecoverThreadReady || rec.ThreadID != "T2" {
		t.Fatalf("the nested fork was not adopted: %+v", rep.ByCall["c2"])
	}
}

// A turn the operator adopts replaces the turn_started the call recorded: the next resume follows the chosen turn.
func TestRecoverAdoptTurnReplacesTheRecordedTurn(t *testing.T) {
	j := heldAtTurnStarted()
	host := &stubHost{turns: map[string][]HostTurn{"t1": {{ID: "uY", Status: "completed", Readable: true, Marker: "c1/t1"}}}}
	res, err := Recover(context.Background(), j, recoverCfg(host, time.Now), RecoverRequest{CallID: "c1", Adopt: "uY"})
	if err != nil || res.TurnID != "uY" {
		t.Fatalf("%+v / %v", res, err)
	}
	st := callStates(j, "r")[0]
	if got := rowStr(st.turnStarted.Data, "turn_id"); got != "uY" {
		t.Errorf("the call still follows turn %q after the operator adopted uY", got)
	}
	if st.held() || !st.open() {
		t.Errorf("held %v open %v after the recovery", st.held(), st.open())
	}
	if info, _ := LookupCall(j, "c1"); info.ThroughTurnID != "uY" {
		t.Errorf("the call's last turn is %q", info.ThroughTurnID)
	}
	if got := threadPrefix(j, "t1", "uY"); len(got) != 1 || got[0]["turn_id"] != "uY" {
		t.Errorf("the replaced turn is still in the prefix of the thread: %v", got)
	}
}

// A thread the operator adopts has to be of the source the backend connects as: a root thread of another source at the
// call's cwd is not this call's, the same identity automatic reconcile lists by.
func TestRecoverAdoptThreadRefusesAnotherSource(t *testing.T) {
	j := heldJournal(EvThreadRequested)
	foreign := HostThread{ID: "t-cli", Cwd: "/w/a1", Source: "cli", CreatedAt: time.UnixMilli(1000), Status: "idle"}
	host := &stubHost{threads: []HostThread{foreign}, turns: map[string][]HostTurn{}}
	before := len(j.Events())
	_, err := Recover(context.Background(), j, recoverCfg(host, time.Now), RecoverRequest{CallID: "c1", Adopt: "t-cli"})
	if !isProofNotMet(err) || !strings.Contains(err.Error(), "source") {
		t.Fatalf("%v", err)
	}
	if len(j.Events()) != before {
		t.Errorf("a refused recovery appended %v", rowKinds(j, before))
	}
}

// The events callstate names are the engine's, and callState.held() is callstate's recovery hold: the one reading `dag-restart`
// and `crw flow recover` share.
func TestCallstateReadsTheEnginesEvents(t *testing.T) {
	for engine, shared := range map[string]string{
		EvPrepared: callstate.Prepared, EvThreadRequested: callstate.ThreadRequested, EvThreadStarted: callstate.ThreadStarted,
		EvForkRequested: callstate.ForkRequested, EvForkStarted: callstate.ForkStarted, EvTurnRequested: callstate.TurnRequested,
		EvTurnStarted: callstate.TurnStarted, EvInterruptRequested: callstate.Interrupt, EvCompleted: callstate.Completed,
		EvNullModel: callstate.NullModel, EvHeld: callstate.Held, EvRefused: callstate.Refused, EvOutcomeUnknown: callstate.OutcomeUnknown,
		EvSuperseded: callstate.Superseded, EvRecoveryCleared: callstate.RecoveryCleared, ReasonDispatchUnknown: callstate.DispatchUnknown,
	} {
		if engine != shared {
			t.Errorf("engine %q, callstate %q", engine, shared)
		}
	}
	row := func(ev string, reason string) Event {
		return Event{Kind: KindCall, CallID: "c1", Op: "agent", Event: ev, Data: map[string]any{"reason": reason}}
	}
	hold := []Event{row(EvPrepared, ""), row(EvThreadRequested, ""), row(EvOutcomeUnknown, ReasonDispatchUnknown)}
	cleared := append(append([]Event(nil), hold...), row(EvRecoveryCleared, ""))
	open := hold[:2]
	for name, c := range map[string]struct {
		rows []Event
		held bool
	}{"hold": {hold, true}, "cleared": {cleared, false}, "open intent": {open, false}} {
		if got := stateOf("c1", c.rows).held(); got != c.held {
			t.Errorf("%s: held() is %v", name, got)
		}
		if got := callstate.Classify(callstateRows(c.rows)) == callstate.RecoveryHold; got != c.held {
			t.Errorf("%s: callstate says hold %v", name, got)
		}
	}
}
