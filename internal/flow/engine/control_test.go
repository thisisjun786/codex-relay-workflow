package engine

import (
	"fmt"
	"strings"
	"testing"
)

// TestControlPauseHoldsTheCalls: a pause asked through Config.Control before the run starts, and one asked while the run goes on, end
// the calls that would send held (engine_paused) and write one run_paused row for each ask; a pause asked after the run ended
// writes nothing.
func TestControlPauseHoldsTheCalls(t *testing.T) {
	h := newHarness(t)
	ctl := &Control{}
	h.cfg.Control = ctl
	ctl.Pause("operator", "crw flow pause")
	res := h.run(`const a = await agent.call('one', { agentType: 'executor', node: 'n1', label: 'a' })
return [a.status, a.reason]`)
	if got := fmt.Sprint(value[[]string](t, res)); got != "[held engine_paused]" {
		t.Fatalf("the call of a paused run is %s", got)
	}
	if res.Status != RunPaused {
		t.Fatalf("a run the operator paused ends %s, want %s", res.Status, RunPaused)
	}
	var paused []Event
	for _, e := range h.journal.Events() {
		if e.Event == EvRunPaused {
			paused = append(paused, e)
		}
	}
	if len(paused) != 1 || paused[0].Data["by"] != "operator" || paused[0].Data["reason"] != "crw flow pause" {
		t.Fatalf("run_paused rows %v", paused)
	}
	before := len(h.journal.Events())
	ctl.Pause("operator", "after the end")
	if len(h.journal.Events()) != before {
		t.Fatal("a pause asked after the run ended wrote a row")
	}
}

// endRowsAfterStart are the KindRun end rows (finished, paused) after the run's last started row.
func endRowsAfterStart(rows []Event) []Event {
	var out []Event
	for _, ev := range rows {
		if ev.Kind != KindRun {
			continue
		}
		switch ev.Event {
		case "started":
			out = nil
		case "finished", "paused":
			out = append(out, ev)
		}
	}
	return out
}

// pausedRowFailingJournal stores nothing from the run's paused end row on and reports it, as a persistent journal whose store went
// away at the end of the run.
type pausedRowFailingJournal struct {
	*MemoryJournal
	failed bool
}

func (f *pausedRowFailingJournal) Append(e Event) Event {
	if e.Kind == KindRun && e.Event == "paused" {
		f.failed = true
	}
	return f.MemoryJournal.Append(e)
}

func (f *pausedRowFailingJournal) Err() error {
	if f.failed {
		return fmt.Errorf("the store went away")
	}
	return nil
}

// TestControlPauseEndsTheRunPaused: a pause the operator asked for ends the run paused - one run/paused row with who asked and why after
// the last started row, no finished row, Result.Status paused - and the upward hook is told one more batch and no finish. A pause the
// run's own limits make (a deadline ask, a token budget, a quota) still ends finished completed, with one RunFinished. A paused end
// whose row could not be stored is a script_error of the engine, never paused.
func TestControlPauseEndsTheRunPaused(t *testing.T) {
	const twoCalls = `const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('two', { agentType: 'explorer', label: 'b' })
return [a.status, b.status, b.reason]`
	t.Run("operator", func(t *testing.T) {
		h := newHarness(t)
		ctl := &Control{}
		hook := &recordingHook{}
		h.cfg.Control, h.cfg.Upward = ctl, hook
		h.fake.Handler = func(r FakeRequest) FakeReply {
			if strings.HasPrefix(r.Prompt, "one") {
				// call a's turn is in flight
				ctl.Pause("operator", "crw flow pause")
			}
			return FakeReply{}
		}
		res := h.run(twoCalls)
		if res.Status != RunPaused || res.ErrorKind != "" {
			t.Fatalf("the run ends %s/%s, want %s", res.Status, res.ErrorKind, RunPaused)
		}
		ends := endRowsAfterStart(h.journal.Events())
		if len(ends) != 1 || ends[0].Event != "paused" || ends[0].Data["by"] != "operator" || ends[0].Data["reason"] != "crw flow pause" || ends[0].Data["status"] != RunPaused {
			t.Fatalf("end rows after the last started %+v, want one paused row by operator", ends)
		}
		if got := strings.Join(hook.calls, ","); got != "started,batch" {
			t.Fatalf("hook calls %s, want started,batch (no RunFinished for a paused end)", got)
		}
		var b string
		for _, c := range res.Calls {
			if c.Label == "b" {
				b = c.Status + "/" + c.Reason
			}
		}
		if b != "held/engine_paused" {
			t.Errorf("the call after the pause is %s", b)
		}
	})
	finishedOnce := func(t *testing.T, h *harness, hook *recordingHook, res *Result) {
		t.Helper()
		if res.Status != RunCompleted {
			t.Fatalf("the run ends %s/%s: %s, want %s", res.Status, res.ErrorKind, res.Error, RunCompleted)
		}
		ends := endRowsAfterStart(h.journal.Events())
		if len(ends) != 1 || ends[0].Event != "finished" || ends[0].Data["status"] != RunCompleted {
			t.Fatalf("end rows after the last started %+v, want one finished completed", ends)
		}
		if got := strings.Join(hook.calls, ","); got != "started,finished:completed" {
			t.Fatalf("hook calls %s, want one RunFinished", got)
		}
	}
	t.Run("deadline", func(t *testing.T) {
		h := newHarness(t)
		ctl := &Control{}
		hook := &recordingHook{}
		h.cfg.Control, h.cfg.Upward = ctl, hook
		h.fake.Handler = func(r FakeRequest) FakeReply {
			if strings.HasPrefix(r.Prompt, "one") {
				ctl.Pause("deadline", "the run's deadline passed")
			}
			return FakeReply{}
		}
		res := h.run(twoCalls)
		finishedOnce(t, h, hook, res)
		if got := fmt.Sprint(value[[]string](t, res)); got != "[held held engine_paused]" && got != "[completed held engine_paused]" {
			t.Errorf("results %s", got)
		}
	})
	t.Run("budget", func(t *testing.T) {
		h := newHarness(t)
		hook := &recordingHook{}
		h.cfg.Upward, h.cfg.Control = hook, &Control{}
		h.cfg.TokenBudget = 100
		h.fake.Handler = func(r FakeRequest) FakeReply { return FakeReply{Tokens: &Tokens{Input: 40, Output: 70}} }
		res := h.run(twoCalls)
		finishedOnce(t, h, hook, res)
		if got := fmt.Sprint(value[[]string](t, res)); got != "[completed held budget_exhausted]" {
			t.Errorf("results %s", got)
		}
	})
	t.Run("quota", func(t *testing.T) {
		h := newHarness(t)
		hook := &recordingHook{}
		h.cfg.Upward, h.cfg.Control = hook, &Control{}
		h.fake.Handler = func(r FakeRequest) FakeReply {
			return FakeReply{Fail: &TurnFailure{CodexErrorInfo: "usageLimitExceeded"}}
		}
		res := h.run(twoCalls)
		finishedOnce(t, h, hook, res)
		if got := fmt.Sprint(value[[]string](t, res)); got != "[held held engine_paused]" {
			t.Errorf("results %s", got)
		}
	})
	t.Run("journal failure", func(t *testing.T) {
		h := newHarness(t)
		j := &pausedRowFailingJournal{MemoryJournal: NewMemoryJournal()}
		ctl := &Control{}
		h.journal, h.cfg.Journal, h.cfg.Control = j.MemoryJournal, j, ctl
		ctl.Pause("operator", "crw flow pause")
		res := h.run(twoCalls)
		if res.Status != RunScriptError || res.ErrorKind != ErrKindEngine {
			t.Fatalf("a paused end whose row could not be stored ends %s/%s, want %s/%s", res.Status, res.ErrorKind, RunScriptError, ErrKindEngine)
		}
	})
}
