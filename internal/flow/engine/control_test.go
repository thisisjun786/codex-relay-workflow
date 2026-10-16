package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
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

// pausingHook asks the run's Control for a pause from inside RunFinished: the end of the run is decided by then, as for a SIGUSR1 that
// comes while the process writes its end row or tells the parent.
type pausingHook struct {
	*recordingHook
	ctl *Control
	by  string
}

func (h *pausingHook) RunFinished(ctx context.Context, run, status string) error {
	h.ctl.Pause(h.by, "late ask")
	return h.recordingHook.RunFinished(ctx, run, status)
}

// endAgrees says whether the rows after the run's last started row agree: a run_paused ask whose asker ends the run paused
// (endsPaused) is there exactly when the end row is run/paused, and there is one end row.
func endAgrees(rows []Event) error {
	asked, ends := false, []string{}
	for _, ev := range rows {
		switch {
		case ev.Kind == KindRun && ev.Event == "started":
			asked, ends = false, nil
		case ev.Kind == KindFlow && ev.Event == EvRunPaused && endsPaused[fmt.Sprint(ev.Data["by"])]:
			asked = true
		case ev.Kind == KindRun && (ev.Event == "finished" || ev.Event == "paused"):
			ends = append(ends, ev.Event+":"+fmt.Sprint(ev.Data["status"]))
		}
	}
	switch {
	case len(ends) != 1:
		return fmt.Errorf("end rows %v, want one", ends)
	case asked && ends[0] != "paused:"+RunPaused:
		return fmt.Errorf("an operator run_paused row is there and the end is %s", ends[0])
	case !asked && ends[0] == "paused:"+RunPaused:
		return fmt.Errorf("the run ended paused with no operator run_paused row")
	}
	return nil
}

// TestControlPauseAfterTheEndIsDecidedRecordsNothing (CRW-1487): once the run has decided how it ends (the end row it writes, the
// completion it tells), a pause is refused: it writes no run_paused row, so the journal never says the operator paused a run that
// finished completed and told its completion. A pause before that decision ends the run paused, and a deadline ask still finishes it
// completed; asked at any moment, the run_paused rows and the end row agree.
func TestControlPauseAfterTheEndIsDecidedRecordsNothing(t *testing.T) {
	const oneCall = `const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
return a.status`
	for _, by := range []string{"operator", "deadline"} {
		t.Run(by+" asked while the run tells its finish", func(t *testing.T) {
			h := newHarness(t)
			ctl := &Control{}
			hook := &pausingHook{recordingHook: &recordingHook{}, ctl: ctl, by: by}
			h.cfg.Control, h.cfg.Upward = ctl, hook
			res := h.run(oneCall)
			if res.Status != RunCompleted {
				t.Fatalf("the run ends %s/%s, want %s", res.Status, res.ErrorKind, RunCompleted)
			}
			if got := strings.Join(hook.calls, ","); got != "started,finished:completed" {
				t.Fatalf("hook calls %s, want started,finished:completed", got)
			}
			for _, ev := range h.journal.Events() {
				if ev.Kind == KindFlow && ev.Event == EvRunPaused {
					t.Fatalf("a pause asked after the end was decided wrote %+v", ev)
				}
			}
			if err := endAgrees(h.journal.Events()); err != nil {
				t.Fatal(err)
			}
		})
	}
	t.Run("operator asked at any moment", func(t *testing.T) {
		for i := 0; i < 60; i++ {
			h := newHarness(t)
			ctl := &Control{}
			h.cfg.Control = ctl
			var wg sync.WaitGroup
			wg.Add(1)
			go func(delay time.Duration) {
				defer wg.Done()
				time.Sleep(delay)
				ctl.Pause("operator", "crw flow pause")
			}(time.Duration(i*50) * time.Microsecond)
			res := h.run(oneCall)
			wg.Wait()
			if err := endAgrees(h.journal.Events()); err != nil {
				t.Fatalf("ask %d (status %s): %v", i, res.Status, err)
			}
			if (res.Status == RunPaused) != (endRowsAfterStart(h.journal.Events())[0].Event == "paused") {
				t.Fatalf("ask %d: status %s and end row disagree", i, res.Status)
			}
		}
	})
}

// TestEndsPausedNamesTheAskersThatEndARunPaused (CRW-1495): the operator's pause ends a run paused and a deadline ask does not, so a
// reader of the rows (cli Summarize) names the operator as who paused a run a later deadline ask also asked.
func TestEndsPausedNamesTheAskersThatEndARunPaused(t *testing.T) {
	if !EndsPaused("operator") {
		t.Errorf("an operator pause does not end a run paused")
	}
	if !EndsPaused("answer") {
		t.Errorf("an answer's drain does not end a run paused")
	}
	if EndsPaused("deadline") {
		t.Errorf("a deadline ask ends a run paused")
	}
}

const drainReason = "crw flow answer: the run stops at its next boundary for an answer"

// drainRun runs two sequential calls whose first turn is in flight (its handler blocks until released) while during asks the Control;
// it returns the result and the journal's rows.
func drainRun(t *testing.T, before, during func(ctl *Control)) (*Result, []Event) {
	t.Helper()
	h := newHarness(t)
	ctl := &Control{}
	hook := &recordingHook{}
	h.cfg.Control, h.cfg.Upward = ctl, hook
	inFlight, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.fake.Handler = func(r FakeRequest) FakeReply {
		if strings.HasPrefix(r.Prompt, "one") {
			once.Do(func() { close(inFlight) })
			<-release
		}
		return FakeReply{Tokens: &Tokens{Input: 11, Output: 7}}
	}
	if before != nil {
		before(ctl)
	}
	done := make(chan *Result, 1)
	go func() {
		done <- h.run(`const a = await agent.call('one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('two', { agentType: 'explorer', label: 'b' })
return [a.status, b.status, b.reason]`)
	}()
	if during != nil {
		select {
		case <-inFlight:
			during(ctl)
		case res := <-done:
			close(release)
			return res, h.journal.Events()
		case <-time.After(10 * time.Second):
			t.Fatal("call a's turn never started")
		}
	}
	close(release)
	select {
	case res := <-done:
		if got := strings.Join(hook.calls, ","); res.Status == RunPaused && strings.Contains(got, "finished") {
			t.Errorf("hook calls %s: a run that ends paused told its finish", got)
		}
		return res, h.journal.Events()
	case <-time.After(15 * time.Second):
		t.Fatal("the drained run did not end")
	}
	return nil, nil
}

// pausedAsks are the run_paused rows as by:reason, and interrupts the interrupt_requested rows.
func pausedAsks(rows []Event) []string {
	var out []string
	for _, ev := range rows {
		if ev.Kind == KindFlow && ev.Event == EvRunPaused {
			out = append(out, fmt.Sprint(ev.Data["by"]))
		}
	}
	return out
}

func interrupts(rows []Event) int { return len(rowsNamed(rows, EvInterruptRequested)) }

func callOf(res *Result, label string) CallResult {
	for _, c := range res.Calls {
		if c.Label == label {
			return c
		}
	}
	return CallResult{}
}

// endPausedBy checks that the run ended paused with one run/paused end row by the asker, and no finished row.
func endPausedBy(t *testing.T, res *Result, rows []Event, by string) {
	t.Helper()
	if res.Status != RunPaused || res.ErrorKind != "" {
		t.Fatalf("the run ends %s/%s: %s, want %s", res.Status, res.ErrorKind, res.Error, RunPaused)
	}
	ends := endRowsAfterStart(rows)
	if len(ends) != 1 || ends[0].Event != "paused" || ends[0].Data["by"] != by || ends[0].Data["status"] != RunPaused {
		t.Fatalf("end rows after the last started %+v, want one paused row by %s", ends, by)
	}
	if err := endAgrees(rows); err != nil {
		t.Fatal(err)
	}
}

// TestControlDrainLetsTheTurnInFlightEnd (CRW-1492): a drain (an answer's ask) stops the run at its next boundary without interrupting
// the turn in flight: that turn ends completed with its tokens, the next call ends held engine_paused, and the run ends paused by
// answer with one run_paused row and no finished row. A drain before the run holds its first call. A pause after a drain interrupts
// the turn and ends the run by the operator; a second drain, or a drain after a pause, writes no row.
func TestControlDrainLetsTheTurnInFlightEnd(t *testing.T) {
	t.Run("drain during the turn", func(t *testing.T) {
		res, rows := drainRun(t, nil, func(ctl *Control) { ctl.Drain("answer", drainReason) })
		if n := interrupts(rows); n != 0 {
			t.Errorf("a drain interrupted the turn in flight (%d interrupt_requested rows)", n)
		}
		if a := callOf(res, "a"); a.Status != StatusCompleted || a.Meta.Tokens.Input != 11 || a.Meta.Tokens.Output != 7 {
			t.Errorf("the turn in flight ends %s/%s with tokens %+v, want completed with its tokens", a.Status, a.Reason, a.Meta.Tokens)
		}
		if b := callOf(res, "b"); b.Status != StatusHeld || b.Reason != ReasonEnginePaused {
			t.Errorf("the call after the drain is %s/%s, want held/engine_paused", b.Status, b.Reason)
		}
		if got := fmt.Sprint(pausedAsks(rows)); got != "[answer]" {
			t.Errorf("run_paused rows by %s, want one by answer", got)
		}
		for _, ev := range rows {
			if ev.Kind == KindFlow && ev.Event == EvRunPaused && ev.Data["reason"] != drainReason {
				t.Errorf("run_paused reason %v", ev.Data["reason"])
			}
		}
		endPausedBy(t, res, rows, "answer")
	})
	t.Run("drain before the run", func(t *testing.T) {
		res, rows := drainRun(t, func(ctl *Control) { ctl.Drain("answer", drainReason) }, nil)
		if a := callOf(res, "a"); a.Status != StatusHeld || a.Reason != ReasonEnginePaused {
			t.Errorf("the first call of a run drained before it started is %s/%s, want held/engine_paused", a.Status, a.Reason)
		}
		if got := fmt.Sprint(pausedAsks(rows)); got != "[answer]" {
			t.Errorf("run_paused rows by %s, want one by answer", got)
		}
		endPausedBy(t, res, rows, "answer")
	})
	t.Run("pause after a drain", func(t *testing.T) {
		res, rows := drainRun(t, nil, func(ctl *Control) {
			ctl.Drain("answer", drainReason)
			ctl.Pause("operator", "crw flow pause")
		})
		if n := interrupts(rows); n != 1 {
			t.Errorf("a pause after a drain wrote %d interrupt_requested rows, want 1", n)
		}
		if got := fmt.Sprint(pausedAsks(rows)); got != "[answer operator]" {
			t.Errorf("run_paused rows by %s, want answer then operator", got)
		}
		endPausedBy(t, res, rows, "operator")
	})
	t.Run("drain twice", func(t *testing.T) {
		res, rows := drainRun(t, nil, func(ctl *Control) {
			ctl.Drain("answer", drainReason)
			ctl.Drain("answer", drainReason)
		})
		if got := fmt.Sprint(pausedAsks(rows)); got != "[answer]" {
			t.Errorf("run_paused rows by %s, want one by answer", got)
		}
		if n := interrupts(rows); n != 0 {
			t.Errorf("%d interrupt_requested rows, want none", n)
		}
		endPausedBy(t, res, rows, "answer")
	})
	t.Run("drain and pause at once", func(t *testing.T) {
		// whichever is accepted first, the operator's pause names the end: a drain accepted before it is applied before it (Control
		// applyM). A guard: the window it closes (between a drain's acceptance and its application) is too narrow to hit on demand
		for i := 0; i < 100; i++ {
			res, rows := drainRun(t, nil, func(ctl *Control) {
				var wg sync.WaitGroup
				wg.Add(2)
				go func() { defer wg.Done(); ctl.Drain("answer", drainReason) }()
				go func() { defer wg.Done(); ctl.Pause("operator", "crw flow pause") }()
				wg.Wait()
			})
			if got := fmt.Sprint(pausedAsks(rows)); got != "[answer operator]" && got != "[operator]" {
				t.Fatalf("ask %d: run_paused rows by %s, want the operator's last", i, got)
			}
			endPausedBy(t, res, rows, "operator")
		}
	})
	t.Run("drain after a pause", func(t *testing.T) {
		res, rows := drainRun(t, nil, func(ctl *Control) {
			ctl.Pause("operator", "crw flow pause")
			ctl.Drain("answer", drainReason)
		})
		if got := fmt.Sprint(pausedAsks(rows)); got != "[operator]" {
			t.Errorf("run_paused rows by %s, want the operator's only", got)
		}
		endPausedBy(t, res, rows, "operator")
	})
}

// TestControlDrainWaitsForAPublishedSessionStep (CRW-1492): a session step published before a drain is in flight: its call passed
// the gate, so the drain neither cuts its wait for the owner nor interrupts the other turn; with the owner taking the step after the
// drain, both calls complete and the run ends paused by answer with one run_paused row.
func TestControlDrainWaitsForAPublishedSessionStep(t *testing.T) {
	k := newSessionKit(t)
	ctl := &Control{}
	k.h.cfg.Control = ctl
	// the owner fake takes steps one at a time, in the order of the calls' step directories: the first call's step it takes at once
	// (its turn runs 200 ms), the second's it takes only once the drain was asked
	var (
		mu      sync.Mutex
		once    sync.Once
		first   string
		release = make(chan struct{})
		second  = make(chan struct{})
		drained = make(chan struct{})
	)
	k.orch.answer = func(st Step, packet string, _ int) orchAnswer {
		mu.Lock()
		if first == "" {
			first = st.Call
		}
		held := st.Call != first
		mu.Unlock()
		if held {
			once.Do(func() { close(second) })
			<-release
		}
		return orchAnswer{after: 200 * time.Millisecond}
	}
	go func() {
		defer close(drained)
		defer close(release)
		select {
		case <-second:
		case <-time.After(10 * time.Second):
			t.Error("no drain point: the owner never came to the second call's step")
			return
		}
		// the second call's step is published and untaken; the first call's turn is in flight once its row is there
		for wait := time.Now().Add(5 * time.Second); time.Now().Before(wait); time.Sleep(time.Millisecond) {
			if len(rowsNamed(k.h.journal.Events(), EvTurnStarted)) > 0 {
				ctl.Drain("answer", drainReason)
				return
			}
		}
		t.Error("no drain point: the first call's turn never started")
	}()
	done := make(chan *Result, 1)
	go func() {
		done <- k.h.run(`const r = await parallel([async () => agent.call('task alpha', { agentType: 'explorer', label: 'a' }), async () => agent.call('task beta', { agentType: 'explorer', label: 'b' })])
return r.map(x => x.status)`)
	}()
	<-drained
	var res *Result
	select {
	case res = <-done:
	case <-time.After(10 * time.Second):
		ctl.Pause("operator", "crw flow pause")
		<-done
		t.Fatalf("the drained run did not end with the owner taking its steps; call a rows %s; call b rows %s",
			eventsOf(k.h.callRows("a")), eventsOf(k.h.callRows("b")))
	}
	rows := k.h.journal.Events()
	if got := fmt.Sprint(value[[]string](t, res)); got != "[completed completed]" {
		t.Errorf("calls %s, want both completed", got)
	}
	if n := interrupts(rows); n != 0 {
		t.Errorf("a drain interrupted a turn (%d interrupt_requested rows)", n)
	}
	if got := fmt.Sprint(pausedAsks(rows)); got != "[answer]" {
		t.Errorf("run_paused rows by %s, want one by answer", got)
	}
	endPausedBy(t, res, rows, "answer")
}
