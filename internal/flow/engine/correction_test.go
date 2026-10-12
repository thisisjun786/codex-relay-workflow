package engine

import (
	"testing"
)

// These tests are the correction round of CRW-1200 and CRW-1269 after their pre-merge evaluation (head 7a793656): each is the
// scenario of one reported defect, written to fail on that head.

func heldRows(h *harness) (held, cleared, prepared int) {
	for _, ev := range h.journal.Events() {
		switch {
		case ev.Kind == KindCall && ev.Event == EvHeld:
			held++
		case ev.Kind == KindFlow && ev.Event == EvHoldCleared:
			cleared++
		case ev.Kind == KindCall && ev.Op == "agent" && ev.Event == EvPrepared:
			prepared++
		}
	}
	return held, cleared, prepared
}

// CRW-1269 d1: a held executor whose hold-time snapshot could not be made (p_held_error) has no state to compare the worktree
// with, so the worktree is not known to be the files the call left: the call is not continued in place.
func TestHeldExecutorWithoutAHoldSnapshotIsNotContinuedInPlace(t *testing.T) {
	h, tf, _ := withTreeFake(t)
	h.fake.Handler = func(r FakeRequest) FakeReply {
		tf.mu.Lock()
		tf.tree = "P:" + r.Op
		tf.snapshotErr = true // the object store goes away while the turn ends
		tf.mu.Unlock()
		return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "serverOverloaded", Message: "busy"}}
	}
	body := `const b = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'b' }); return [b.status]`
	if got := value[[]string](t, h.run(body)); got[0] != "held" {
		t.Fatalf("%v", got)
	}
	var held Event
	for _, ev := range h.journal.Events() {
		if ev.Event == EvHeld {
			held = ev
		}
	}
	if rowStr(held.Data, "p_held_error") == "" || rowStr(held.Data, "p_held") != "" {
		t.Fatalf("the held row %v: the scenario needs a hold whose snapshot failed", held.Data)
	}
	h.fake.Handler = func(r FakeRequest) FakeReply {
		tf.setTree("P:" + r.Op)
		return FakeReply{}
	}
	tf.mu.Lock()
	tf.snapshotErr = false
	tf.mu.Unlock()
	if got := value[[]string](t, h.resumeOn(body)); got[0] != "completed" {
		t.Fatalf("%v", got)
	}
	if _, cleared, prepared := heldRows(h); cleared != 0 || prepared != 2 {
		t.Errorf("%d hold_cleared rows and %d prepared rows: the held call went on over files nobody proved, want a new attempt", cleared, prepared)
	}
}

// CRW-1200 d3: an operation that runs again because its earlier attempt was superseded (its arguments changed, or a position
// before it did not replay) is a new request with a request id of its own: a receiver that deduplicates by request id would
// otherwise return the earlier answer and make no effect. A run that only repeats an attempt that left no row keeps the id.
func TestSupersededOperationAttemptsGetDistinctRequestIDs(t *testing.T) {
	h, _, w := withTreeFake(t)
	script := func(earlier, arg string) string {
		return `const e = await agent.call('` + earlier + `', { agentType: 'explorer', label: 'e' })
const i = await crw.integrate('n1', '` + arg + `')
return [i.id]`
	}
	first := value[[]string](t, h.run(script("earlier", "A")))
	if w.count("integrate") != 1 {
		t.Fatalf("%d effects after the first run", w.count("integrate"))
	}
	// the operation's own arguments changed
	second := value[[]string](t, h.resumeOn(script("earlier", "B")))
	if n := w.count("integrate"); n != 2 {
		t.Errorf("%d effects after the arguments changed: the receiver took the new request for the old one", n)
	}
	if second[0] == first[0] {
		t.Errorf("the new attempt answered %s, the earlier attempt's answer", second[0])
	}
	// a position before it did not replay: the same arguments, a new attempt in the suffix
	third := value[[]string](t, h.resumeOn(script("earlier, edited", "B")))
	if n := w.count("integrate"); n != 3 {
		t.Errorf("%d effects after an earlier position changed: the suffix's operation was taken for the earlier one", n)
	}
	if third[0] == second[0] {
		t.Errorf("the suffix's attempt answered %s, the earlier attempt's answer", third[0])
	}
	// nothing changed: every position replays and nothing runs
	runs := w.ran("integrate")
	if again := value[[]string](t, h.resumeOn(script("earlier, edited", "B"))); again[0] != third[0] {
		t.Errorf("an unchanged resume returned %s, want %s", again[0], third[0])
	}
	if w.ran("integrate") != runs || w.count("integrate") != 3 {
		t.Errorf("an unchanged resume ran the operation again")
	}
}
