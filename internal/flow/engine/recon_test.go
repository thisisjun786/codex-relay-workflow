package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// A call whose outcome is unknown holds its node: no other call of the node is sent, a continuation of its handle is not
// sent, a tier step for the node is refused, and a node nothing is wrong with goes on.
func TestUnknownNeverTierStepsOrRetries(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if strings.Contains(r.Prompt, "lose it") {
			return FakeReply{Err: &TransportError{Err: errors.New("the answer was lost")}}
		}
		return FakeReply{}
	}
	res := h.run(`
const a = await agent.call('lose it', { agentType: 'executor', node: 'n1', label: 'a' })
const b = await agent.call('retry it', { agentType: 'executor', node: 'n1', label: 'b' })
let tier = 'no error'
try { await crw.tierStep('n1', { evidence: [a.callId] }) } catch (e) { tier = String(e) }
const c = await agent.call('continue it', { resume: a, label: 'c' })
const other = await agent.call('another node', { agentType: 'executor', node: 'n2', label: 'o' })
return { rs: [a, b, c, other].map((r) => r.status + '/' + r.reason), tier }
`)
	v := value[struct {
		Rs   []string `json:"rs"`
		Tier string   `json:"tier"`
	}](t, res)
	want := []string{"unknown/dispatch_unknown", "held/recovery_hold", "held/recovery_hold", "completed/null"}
	if !slices.Equal(v.Rs, want) {
		t.Fatalf("%v, want %v", v.Rs, want)
	}
	if !strings.Contains(v.Tier, "recovery_hold") {
		t.Errorf("the tier step for the held node: %q", v.Tier)
	}
	// the fake saw the lost call's thread and turn and the other node's thread and turn: nothing for b, nothing for c
	var threads, turns int
	for _, r := range h.fake.Requests() {
		switch r.Kind {
		case "thread":
			threads++
		case "turn":
			turns++
		}
	}
	if threads != 2 || turns != 2 {
		t.Errorf("%d thread and %d turn requests, want 2 and 2", threads, turns)
	}
	for _, label := range []string{"b", "c"} {
		rows := h.callRows(label)
		for _, r := range rows {
			if r.Event == EvThreadRequested || r.Event == EvTurnRequested {
				t.Errorf("%s wrote %s on a held node", label, r.Event)
			}
		}
	}
	var steps int
	for _, e := range h.journal.Events() {
		if e.Op == "crw.tierStep" {
			steps++
		}
	}
	if steps != 0 {
		t.Errorf("%d tier steps were journaled", steps)
	}
	var escalations []Event
	for _, e := range h.journal.Events() {
		if e.Kind == KindFlow && e.Event == EvEscalation && e.Data["reason"] == ReasonDispatchUnknown {
			escalations = append(escalations, e)
		}
	}
	if len(escalations) != 1 || escalations[0].Data["node"] != "n1" {
		t.Errorf("escalations %v", escalations)
	}
}

// stubHost is a host whose threads and turns a test lays out.
type stubHost struct {
	mu        sync.Mutex
	threads   []HostThread
	turns     map[string][]HostTurn
	listErr   error
	turnsErr  map[string]error
	creation  Creation
	archived  []string
	listCalls int
}

func (s *stubHost) ListThreads(ctx context.Context, cwd string) ([]HostThread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.listCalls++
	if s.listErr != nil {
		return nil, s.listErr
	}
	var out []HostThread
	for _, th := range s.threads {
		if th.Cwd == cwd {
			out = append(out, th)
		}
	}
	return out, nil
}

func (s *stubHost) ReadThread(ctx context.Context, id string) (HostThread, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, th := range s.threads {
		if th.ID == id {
			return th, nil
		}
	}
	return HostThread{}, errors.New("thread not found")
}

func (s *stubHost) Turns(ctx context.Context, thread string) ([]HostTurn, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.turnsErr[thread]; err != nil {
		return nil, err
	}
	return s.turns[thread], nil
}

func (s *stubHost) ArchiveThread(ctx context.Context, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.archived = append(s.archived, id)
	return nil
}

func (s *stubHost) InterruptTurn(ctx context.Context, thread, turn string) error { return nil }

func (s *stubHost) HarvestTurn(ctx context.Context, thread, turn, op string) (TurnOutcome, bool, error) {
	return TurnOutcome{}, false, errors.New("not implemented")
}

func (s *stubHost) AwaitTurn(ctx context.Context, thread, turn, op string, tools *RepositoryTools) (TurnOutcome, error) {
	return TurnOutcome{}, errors.New("not implemented")
}

func (s *stubHost) CreationState(owner, callID string, send int) Creation {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creation
}

// heldJournal is the journal of a run whose call c1 stopped at the intent (thread_requested, or turn_requested) and whose
// outcome reconcile left unknown.
func heldJournal(intent string) *MemoryJournal {
	j := NewMemoryJournal()
	add := func(ev string, data map[string]any) {
		j.Append(Event{RunID: "r", Kind: KindCall, CallID: "c1", Path: "root", Position: 1, Op: "agent", Label: "a", Event: ev, Data: data})
	}
	add(EvPrepared, map[string]any{"role": "executor", "node": "n1"})
	switch intent {
	case EvThreadRequested:
		add(EvThreadRequested, map[string]any{"cwd": "/w/a1", "send": 1, "at": int64(1000), "owner": "p1"})
	case EvTurnRequested:
		add(EvThreadRequested, map[string]any{"cwd": "/w/a1", "send": 1, "at": int64(1000), "owner": "p1"})
		add(EvThreadStarted, map[string]any{"thread_id": "t1", "model": "m", "effort": "e", "cwd": "/w/a1"})
		add(EvTurnRequested, map[string]any{"k": 1, "op": "c1/t1", "input": "go"})
	}
	add(EvOutcomeUnknown, map[string]any{"reason": ReasonDispatchUnknown, "detail": "unknown"})
	return j
}

func recoverCfg(host ReconHost, now func() time.Time) ReconcileConfig {
	return ReconcileConfig{RunID: "r", Host: host, Now: now}
}

func rowKinds(j Journal, from int) []string {
	var out []string
	for _, e := range j.Events()[from:] {
		out = append(out, e.Event)
	}
	return out
}

func isProofNotMet(err error) bool {
	var p *ProofNotMet
	return errors.As(err, &p)
}

// `crw flow recover CALL --absent` accepts a call only when the proofs of the automatic reconcile hold now - only turnless
// candidates, an exact-prefix fork, or no candidate after the connection closed and the grace passed, and for a turn no
// marker turn on an idle thread - and records the evidence; when they do not it appends nothing and says why.
func TestRecoverAbsentRequiresProof(t *testing.T) {
	clock := time.UnixMilli(2_000_000)
	now := func() time.Time { return clock }
	candidate := HostThread{ID: "t-new", Cwd: "/w/a1", Source: "appServer", CreatedAt: time.UnixMilli(1000), Status: "idle"}

	t.Run("one request names a mode", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		for _, req := range []RecoverRequest{{CallID: "c1"}, {CallID: "c1", Absent: true, Adopt: "t-new"}} {
			if _, err := Recover(context.Background(), j, recoverCfg(&stubHost{}, now), req); err == nil || isProofNotMet(err) {
				t.Errorf("%+v: %v", req, err)
			}
		}
		if _, err := Recover(context.Background(), j, recoverCfg(&stubHost{}, now), RecoverRequest{CallID: "nope", Absent: true}); err == nil {
			t.Errorf("a call the run does not have was recovered")
		}
	})
	t.Run("a call that is not held", func(t *testing.T) {
		j := NewMemoryJournal()
		j.Append(Event{RunID: "r", Kind: KindCall, CallID: "c1", Op: "agent", Event: EvPrepared, Data: map[string]any{"node": "n1"}})
		j.Append(Event{RunID: "r", Kind: KindCall, CallID: "c1", Op: "agent", Event: EvCompleted, Data: map[string]any{}})
		if _, err := Recover(context.Background(), j, recoverCfg(&stubHost{}, now), RecoverRequest{CallID: "c1", Absent: true}); !isProofNotMet(err) {
			t.Errorf("%v", err)
		}
	})
	t.Run("a thread with turns refuses", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		host := &stubHost{threads: []HostThread{candidate}, turns: map[string][]HostTurn{"t-new": {{ID: "u1", Status: "completed", Readable: true, Marker: "other/t1"}}}}
		before := len(j.Events())
		_, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true})
		if !isProofNotMet(err) || !strings.Contains(err.Error(), "holds 1 turn") {
			t.Fatalf("%v", err)
		}
		if len(j.Events()) != before || len(host.archived) != 0 {
			t.Errorf("a refused recovery appended %v and archived %v", rowKinds(j, before), host.archived)
		}
	})
	t.Run("a listing that cannot be completed refuses", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		before := len(j.Events())
		_, err := Recover(context.Background(), j, recoverCfg(&stubHost{listErr: errors.New("page 2 failed")}, now), RecoverRequest{CallID: "c1", Absent: true})
		if !isProofNotMet(err) || len(j.Events()) != before {
			t.Errorf("%v / %v", err, rowKinds(j, before))
		}
	})
	t.Run("an absence without the proof refuses", func(t *testing.T) {
		for _, state := range []Creation{CreationOpen, CreationUnknown} {
			j := heldJournal(EvThreadRequested)
			before := len(j.Events())
			_, err := Recover(context.Background(), j, recoverCfg(&stubHost{creation: state}, now), RecoverRequest{CallID: "c1", Absent: true})
			if !isProofNotMet(err) || len(j.Events()) != before {
				t.Errorf("creation state %d: %v / %v", state, err, rowKinds(j, before))
			}
		}
	})
	t.Run("the grace after the connection closed", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		host := &stubHost{creation: CreationClosed}
		before := len(j.Events())
		_, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true})
		if !isProofNotMet(err) || !strings.Contains(err.Error(), "second complete listing is due") {
			t.Fatalf("%v", err)
		}
		if got := rowKinds(j, before); !slices.Equal(got, []string{EvCreationClosedSeen}) {
			t.Errorf("rows %v: only the sighting of the closed connection is recorded", got)
		}
		clock = clock.Add(DefaultCreationGrace)
		before = len(j.Events())
		res, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true})
		if err != nil || res.Action != RecoverResendCreate {
			t.Fatalf("%+v / %v", res, err)
		}
		if got := rowKinds(j, before); !slices.Equal(got, []string{EvRecoveryCleared, EvCreationResendable}) {
			t.Errorf("rows %v", got)
		}
		cleared := j.Events()[before]
		if cleared.Data["mode"] != "absent" || cleared.Data["evidence"] == nil {
			t.Errorf("recovery_cleared %v", cleared.Data)
		}
	})
	t.Run("a turnless candidate is accepted with its evidence", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		host := &stubHost{threads: []HostThread{candidate}, turns: map[string][]HostTurn{}}
		before := len(j.Events())
		res, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true})
		if err != nil || res.Action != RecoverThreadReady || res.ThreadID != "t-new" {
			t.Fatalf("%+v / %v", res, err)
		}
		if got := rowKinds(j, before); !slices.Equal(got, []string{EvRecoveryCleared, EvThreadStarted}) {
			t.Errorf("rows %v", got)
		}
		if started := j.Events()[before+1]; started.Data["recovered"] != true || started.Data["thread_id"] != "t-new" {
			t.Errorf("thread_started %v", started.Data)
		}
		// the hold is over: the call is open at an adopted thread, and a second recovery has nothing to release
		if _, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true}); !isProofNotMet(err) {
			t.Errorf("%v", err)
		}
		if len(RecoveryHolds(j, "r")) != 0 {
			t.Errorf("holds %v", RecoveryHolds(j, "r"))
		}
	})
	t.Run("a turn: an active thread refuses, an idle one without the marker allows the turn again", func(t *testing.T) {
		j := heldJournal(EvTurnRequested)
		host := &stubHost{threads: []HostThread{{ID: "t1", Cwd: "/w/a1", Status: "active"}}, turns: map[string][]HostTurn{"t1": {{ID: "u1", Status: "inProgress", Readable: true}}}}
		before := len(j.Events())
		if _, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true}); !isProofNotMet(err) || len(j.Events()) != before {
			t.Fatalf("%v / %v", err, rowKinds(j, before))
		}
		host.threads[0].Status = "idle"
		host.turns["t1"] = []HostTurn{{ID: "u1", Status: "interrupted", Readable: true}}
		res, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true})
		if err != nil || res.Action != RecoverResendTurn {
			t.Fatalf("%+v / %v", res, err)
		}
		// an unreadable turn is not an absence
		j2 := heldJournal(EvTurnRequested)
		host.turns["t1"] = []HostTurn{{ID: "u1", Status: "completed", ReadError: errors.New("not loaded")}}
		if _, err := Recover(context.Background(), j2, recoverCfg(host, now), RecoverRequest{CallID: "c1", Absent: true}); !isProofNotMet(err) {
			t.Errorf("%v", err)
		}
	})
	t.Run("adopt takes the thread or turn the operator names", func(t *testing.T) {
		j := heldJournal(EvThreadRequested)
		elsewhere := HostThread{ID: "t-else", Cwd: "/w/other", CreatedAt: time.UnixMilli(1000)}
		host := &stubHost{threads: []HostThread{candidate, elsewhere}, turns: map[string][]HostTurn{"t-new": {{ID: "u1", Status: "completed", Readable: true}}}}
		before := len(j.Events())
		if _, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Adopt: "t-else"}); !isProofNotMet(err) || len(j.Events()) != before {
			t.Fatalf("%v", err)
		}
		res, err := Recover(context.Background(), j, recoverCfg(host, now), RecoverRequest{CallID: "c1", Adopt: "t-new"})
		if err != nil || res.Action != RecoverThreadReady {
			t.Fatalf("%+v / %v", res, err)
		}
		if got := rowKinds(j, before); !slices.Equal(got, []string{EvRecoveryCleared, EvThreadStarted}) {
			t.Errorf("rows %v", got)
		}
		if cleared := j.Events()[before]; cleared.Data["mode"] != "adopt" || fmt.Sprint(cleared.Data["evidence"]) == "" {
			t.Errorf("recovery_cleared %v", cleared.Data)
		}
		// a turn
		jt := heldJournal(EvTurnRequested)
		hostT := &stubHost{turns: map[string][]HostTurn{"t1": {{ID: "u1", Status: "completed", Readable: true, Marker: "other/t1"}, {ID: "u2", Status: "completed", Readable: true, Marker: "c1/t1"}}}}
		before = len(jt.Events())
		if _, err := Recover(context.Background(), jt, recoverCfg(hostT, now), RecoverRequest{CallID: "c1", Adopt: "u1"}); !isProofNotMet(err) {
			t.Errorf("a turn of another operation was adopted: %v", err)
		}
		if _, err := Recover(context.Background(), jt, recoverCfg(hostT, now), RecoverRequest{CallID: "c1", Adopt: "nope"}); !isProofNotMet(err) {
			t.Errorf("a turn the thread does not have was adopted: %v", err)
		}
		res, err = Recover(context.Background(), jt, recoverCfg(hostT, now), RecoverRequest{CallID: "c1", Adopt: "u2"})
		if err != nil || res.Action != RecoverTurn || res.TurnID != "u2" {
			t.Fatalf("%+v / %v", res, err)
		}
		if got := rowKinds(jt, before); !slices.Equal(got, []string{EvRecoveryCleared, EvTurnStarted}) {
			t.Errorf("rows %v", got)
		}
	})
}

// A resumed run does not send a call again that the journal ended with the same input, and the hold of a node with an
// unknown outcome outlasts the process: the held node takes no new call in the resume either. A call whose input changed
// runs live.
func TestResumeDoesNotRunEndedCallsAgain(t *testing.T) {
	h := newHarness(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if strings.Contains(r.Prompt, "lose it") {
			return FakeReply{Err: &TransportError{Err: errors.New("the answer was lost")}}
		}
		return FakeReply{}
	}
	body := func(build string) string {
		return `
const a = await agent.call('` + build + `', { agentType: 'executor', node: 'n1', label: 'a' })
const b = await agent.call('lose it', { agentType: 'executor', node: 'n2', label: 'b' })
const c = await agent.call('next on n2', { agentType: 'executor', node: 'n2', label: 'c' })
return [a, b, c].map((r) => r.status + '/' + r.reason)`
	}
	first := value[[]string](t, h.run(body("build")))
	want := []string{"completed/null", "unknown/dispatch_unknown", "held/recovery_hold"}
	if !slices.Equal(first, want) {
		t.Fatalf("%v", first)
	}
	sent := len(h.fake.Requests())
	h.cfg.Resume = &ResumeOptions{}
	second := value[[]string](t, h.run(body("build")))
	if !slices.Equal(second, want) {
		t.Errorf("the resume answered %v, want %v", second, want)
	}
	if n := len(h.fake.Requests()); n != sent {
		t.Errorf("the resume sent %d more requests: an ended call is replayed, an unknown one stays unknown, a held node takes nothing", n-sent)
	}
	if got := RecoveryHolds(h.journal, "run-test"); len(got["n2"]) != 1 {
		t.Errorf("holds %v", got)
	}
	// a changed input is another call: it runs
	third := value[[]string](t, h.run(body("build differently")))
	if !slices.Equal(third, want) {
		t.Errorf("%v", third)
	}
	if n := len(h.fake.Requests()); n != sent+2 {
		t.Errorf("%d requests after a changed input, want the changed call's thread and turn on top of %d", n, sent)
	}
}

// A reconcile pass whose context ends decides nothing: the error it read is the cancellation, not the host, so no
// outcome_unknown is written for it.
func TestCancelledReconcileDecidesNothing(t *testing.T) {
	j := NewMemoryJournal()
	add := func(ev string, data map[string]any) {
		j.Append(Event{RunID: "r", Kind: KindCall, CallID: "c1", Path: "root", Position: 1, Op: "agent", Label: "a", Event: ev, Data: data})
	}
	add(EvPrepared, map[string]any{"role": "executor", "node": "n1"})
	add(EvThreadRequested, map[string]any{"cwd": "/w/a1", "send": 1, "at": int64(1000), "owner": "p1"})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := len(j.Events())
	host := &stubHost{listErr: ctx.Err(), creation: CreationClosed}
	rep, err := Reconcile(ctx, j, ReconcileConfig{RunID: "r", Host: host})
	if !errors.Is(err, context.Canceled) || len(rep.ByCall) != 0 || len(j.Events()) != before {
		t.Errorf("%v / %v / rows %v", err, rep.ByCall, rowKinds(j, before))
	}
}
