package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-1384: the five session-engine candidates the V5 review of CRW-1331's pre-fix head named (B1 interrupt intent, B2 repair
// previous_answer, B3 held resume firstK, A4+B4 threadPrefix chain, A5 startCommit), each as a test that fails when the candidate
// is real. The runs are the session runs of session_fake_test.go; a run whose process dies is resumed over the same journal.

const e2aBuild = `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`

// e2aKit is a session run whose first process dies after the row stop names and whose second process resumes it over the same
// memory journal, bus and host.
type e2aKit struct {
	*sessionKit
	sw  *deadSwitch
	mem *MemoryJournal
	// mu guards phase, which the orchestrator's answer reads: 1 while the first process runs, 2 after it.
	mu    sync.Mutex
	phase int
}

func newE2aKit(t *testing.T, stop func(Event) bool) *e2aKit {
	t.Helper()
	k := &e2aKit{sessionKit: newSessionKit(t), sw: &deadSwitch{}, mem: NewMemoryJournal(), phase: 1}
	k.h.journal = k.mem
	k.h.cfg.Journal = &dyingJournal{Journal: k.mem, sw: k.sw, stop: stop}
	k.cfg.Bus = &dyingBus{FileStepBus: k.bus, sw: k.sw}
	k.cfg.Host = &dyingHost{fakeSessionHost: k.host, sw: k.sw}
	return k
}

func (k *e2aKit) phaseNow() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.phase
}

// die runs the first process until it dies.
func (k *e2aKit) die(body string) {
	k.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	ctx1, cancel1 := context.WithCancel(ctx)
	k.sw.mu.Lock()
	k.sw.cancel = cancel1
	k.sw.mu.Unlock()
	if _, err := Run(ctx1, k.h.load(body), k.h.cfg); err != nil {
		k.t.Fatal(err)
	}
	if !k.sw.isDead() {
		k.t.Fatalf("the process never died; rows %s", eventsOf(k.rowsOf("exe")))
	}
	k.mu.Lock()
	k.phase = 2
	k.mu.Unlock()
}

// resume runs the process that resumes the run, as crw flow resume does (CRW-1278: it counts what the journal shows spent).
func (k *e2aKit) resume(body string) *Result {
	k.t.Helper()
	k.h.cfg.Journal = k.mem
	k.cfg.Bus = k.bus
	k.cfg.Host = k.host
	k.h.cfg.Control = nil
	k.h.cfg.Resume = &ResumeOptions{}
	k.h.cfg.SpentBefore = RunSpent(k.mem.Events(), k.h.cfg.RunID).Total
	return k.h.run(body)
}

// pauseWhen pauses the first process once cond holds on its rows.
func (k *e2aKit) pauseWhen(cond func([]Event) bool) {
	ctl := &Control{}
	k.h.cfg.Control = ctl
	go func() {
		for i := 0; i < 10000; i++ {
			if cond(k.mem.Events()) {
				ctl.Pause("operator", "test")
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
}

// B1: an engine interrupt the previous process asked for belongs to the turn it interrupted. A new attempt after that turn's
// attempt was lost, and a step no subagent was bound to when the interrupt came (the engine interrupted nothing), are turns the
// resumed process sends: a subagent that completes one completes the call, and is never held "after the engine interrupted it".
func TestSessionInterruptIntentStaysWithTheTurnItInterrupted(t *testing.T) {
	t.Run("bound turn lost after the restart", func(t *testing.T) {
		k := newE2aKit(t, deathAt(EvInterruptRequested))
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			if k.phaseNow() == 1 {
				return orchAnswer{hang: true}
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		k.pauseWhen(func(rows []Event) bool { return len(rowsNamed(rows, EvTurnStarted)) > 0 })
		k.die(e2aBuild)
		// the host lost the interrupted subagent with the process: the resumed call loses the attempt and spawns the next
		k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) { a.gone = true })
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if got[0] != "completed" {
			t.Fatalf("the resumed call %v; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
		if want := "prepared,step_published,thread_started,turn_started,interrupt_requested,session_attempt_lost,step_published,thread_started,turn_started,completed"; eventsOf(rows) != want {
			t.Errorf("rows %s, want %s", eventsOf(rows), want)
		}
	})
	t.Run("lost attempt recorded before the restart", func(t *testing.T) {
		k := newE2aKit(t, deathAt(EvSessionAttemptLost))
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			if k.phaseNow() == 1 {
				return orchAnswer{hang: true}
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		// the interrupt is asked for the bound turn, and its attempt is lost before the process ends it (the host lost the thread
		// while the interrupt was on its way): the dead process stored interrupt_requested and session_attempt_lost
		k.h.cfg.Journal = &interruptThenLose{dyingJournal: k.h.cfg.Journal.(*dyingJournal), host: k.host}
		k.pauseWhen(func(rows []Event) bool { return len(rowsNamed(rows, EvTurnStarted)) > 0 })
		k.die(e2aBuild)
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if got[0] != "completed" {
			t.Fatalf("the resumed call %v; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
	})
	t.Run("unbound spawn step", func(t *testing.T) {
		k := newE2aKit(t, deathAt(EvInterruptRequested))
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			// the owner takes the step only after the process that published it asked for the interrupt and died
			for k.phaseNow() == 1 {
				time.Sleep(time.Millisecond)
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		k.pauseWhen(func(rows []Event) bool { return len(rowsNamed(rows, EvStepPublished)) > 0 })
		k.die(e2aBuild)
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if got[0] != "completed" {
			t.Fatalf("the resumed call %v; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
		if want := "prepared,step_published,interrupt_requested,thread_started,turn_started,completed"; eventsOf(rows) != want {
			t.Errorf("rows %s, want %s", eventsOf(rows), want)
		}
	})
}

// interruptThenLose makes the host lose the bound subagent the moment the interrupt row is stored and holds the interrupt (its
// cancellation follows the row) until the harvest has recorded the attempt lost: the dying process stores interrupt_requested and
// then session_attempt_lost, as a process does whose host lost the thread while the interrupt was on its way.
type interruptThenLose struct {
	*dyingJournal
	host *fakeSessionHost
}

func (j *interruptThenLose) Append(e Event) Event {
	out := j.dyingJournal.Append(e)
	if e.Kind == KindCall && e.Event == EvInterruptRequested {
		j.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) { a.gone = true })
		for i := 0; i < 5000 && !j.sw.isDead(); i++ {
			time.Sleep(time.Millisecond)
		}
	}
	return out
}

// The B1 seam, its one rule: an interrupt the previous process asked for decides only the turn it was asked for, the turn bound to
// the step and attempt its row names. A repair step (send_input) the previous process published and the host had not taken when the
// interrupt came is a turn the resumed process sends, whether the owner takes the step this process publishes again or the file the
// dead process left: its completed answer completes the call. The turn the interrupt reached - a bound turn, or the repair turn the
// stop bound before it interrupted it - is held engine_paused, however it ended.
func TestSessionInterruptIntentDecidesOnlyItsTurn(t *testing.T) {
	repairPublished := func(rows []Event) bool {
		for _, r := range rowsNamed(rows, EvStepPublished) {
			if rowInt(r.Data, "k") == 2 {
				return true
			}
		}
		return false
	}
	t.Run("repair step the owner had not taken", func(t *testing.T) {
		k := newE2aKit(t, deathAt(EvInterruptRequested))
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			switch {
			case st.K == 1:
				return orchAnswer{text: "this is not JSON"}
			case k.phaseNow() == 1:
				// the owner never takes the repair step the dead process published
				return orchAnswer{skip: true}
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		k.pauseWhen(repairPublished)
		k.die(e2aBuild)
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if got[0] != "completed" {
			t.Fatalf("the resumed call %v; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
		if want := "prepared,step_published,thread_started,turn_started,step_published,interrupt_requested,turn_started,completed"; eventsOf(rows) != want {
			t.Errorf("rows %s, want %s", eventsOf(rows), want)
		}
		if inputs := k.orch.sentInput(); len(inputs) != 1 {
			t.Errorf("repair inputs %+v, want the one the resumed process published", inputs)
		}
	})
	t.Run("repair step the owner takes after the restart", func(t *testing.T) {
		k := newE2aKit(t, deathAt(EvInterruptRequested))
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			if st.K == 1 {
				return orchAnswer{text: "this is not JSON"}
			}
			// the owner takes the file the dead process left, once that process is gone
			for k.phaseNow() == 1 {
				time.Sleep(time.Millisecond)
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		k.pauseWhen(repairPublished)
		k.die(e2aBuild)
		for i := 0; i < 5000 && len(k.orch.sentInput()) == 0; i++ {
			time.Sleep(time.Millisecond)
		}
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if got[0] != "completed" {
			t.Fatalf("the resumed call %v; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
		if inputs := k.orch.sentInput(); len(inputs) != 1 {
			t.Errorf("repair inputs %+v, want the one the owner took from the dead process's step", inputs)
		}
	})
	t.Run("repair turn the stop interrupted", func(t *testing.T) {
		k := newE2aKit(t, nil)
		// the process dies the moment its stop has interrupted the repair turn, before it ends the call
		k.cfg.Host = &killOnInterrupt{dyingHost: k.cfg.Host.(*dyingHost)}
		ctl := &Control{}
		k.h.cfg.Control = ctl
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			if st.K == 1 {
				return orchAnswer{text: "this is not JSON"}
			}
			return orchAnswer{hang: true}
		}
		// the run is paused the moment the owner has sent the repair, before it reports it
		k.orch.onInput = func(Step, string) {
			if k.phaseNow() == 1 {
				ctl.Pause("operator", "test")
			}
		}
		k.die(e2aBuild)
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if fmt.Sprint(got) != "[held engine_paused]" {
			t.Fatalf("the resumed call %v, want held engine_paused; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
		var bound []string
		for _, r := range rowsNamed(rows, EvTurnStarted) {
			bound = append(bound, rowStr(r.Data, "turn_id"))
		}
		if strings.Join(bound, ",") != "agent-1-turn1,agent-1-turn2" {
			t.Errorf("bound turns %v, want the task turn and the repair turn the stop interrupted; rows %s", bound, eventsOf(rows))
		}
		if inputs := k.orch.sentInput(); len(inputs) != 1 {
			t.Errorf("repair inputs %+v, want one", inputs)
		}
	})
	t.Run("bound turn that ends after the restart", func(t *testing.T) {
		k := newE2aKit(t, deathAt(EvInterruptRequested))
		k.orch.answer = func(Step, string, int) orchAnswer { return orchAnswer{hang: true} }
		k.pauseWhen(func(rows []Event) bool { return len(rowsNamed(rows, EvTurnStarted)) > 0 })
		k.die(e2aBuild)
		// the turn the interrupt was asked for ends with a valid answer after the process that asked for it died
		packet, _ := os.ReadFile(k.orch.spawned()[0].Packet)
		answer := defaultMessage("", packetSchema(string(packet)))
		k.host.set("agent-1", func(a *fakeAgent, turn *SessionTurn) {
			turn.Status, turn.FinalText, turn.FinalPhase, turn.UpdatedAt = "completed", answer, "final_answer", time.Now()
			a.Status = "idle"
		})
		res := k.resume(e2aBuild)
		got := value[[]any](t, res)
		rows := k.rowsOf("exe")
		if fmt.Sprint(got) != "[held engine_paused]" {
			t.Fatalf("the resumed call %v, want held engine_paused; rows %s; last row %v", got, eventsOf(rows), rows[len(rows)-1].Data)
		}
		if want := "prepared,step_published,thread_started,turn_started,interrupt_requested,held"; eventsOf(rows) != want {
			t.Errorf("rows %s, want %s", eventsOf(rows), want)
		}
		if d := rowStr(rows[len(rows)-1].Data, "detail"); d != "the turn ended completed after the engine interrupted it" {
			t.Errorf("held detail %q", d)
		}
	})
}

// killOnInterrupt is the host of a process that dies right after it interrupted a turn.
type killOnInterrupt struct {
	*dyingHost
}

func (h *killOnInterrupt) Interrupt(ctx context.Context, id, turn string) error {
	err := h.dyingHost.Interrupt(ctx, id, turn)
	h.sw.kill()
	return err
}

// B2: a repair turn sent to the open subagent records the answer it repairs, so a resumed process whose repair attempt is lost
// gives the next attempt's packet "## Your previous answer", from the bound stage and from the lost stage alike.
func TestSessionRepairKeepsThePreviousAnswerAcrossARestart(t *testing.T) {
	for _, tc := range []struct {
		name string
		stop func(Event) bool
		// lose loses the repair attempt in the first process (it records session_attempt_lost before it dies)
		lose bool
	}{
		{name: "repair turn_started", stop: deathAtK(EvTurnStarted, 2)},
		{name: "repair attempt lost", stop: deathAt(EvSessionAttemptLost), lose: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newE2aKit(t, tc.stop)
			k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
				switch {
				case st.K == 1:
					return orchAnswer{text: "this is not JSON"}
				case k.phaseNow() == 1 && tc.lose:
					return orchAnswer{lose: true}
				case k.phaseNow() == 1:
					return orchAnswer{hang: true}
				}
				return orchAnswer{edit: "P:" + st.Op}
			}
			k.die(e2aBuild)
			k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) { a.gone = true })
			res := k.resume(e2aBuild)
			got := value[[]any](t, res)
			rows := k.rowsOf("exe")
			if got[0] != "completed" {
				t.Fatalf("the resumed call %v; rows %s", got, eventsOf(rows))
			}
			spawns := k.orch.spawned()
			if len(spawns) != 2 || spawns[1].K != 2 || spawns[1].Attempt != 2 {
				t.Fatalf("spawns %+v, want the repair turn's second attempt", spawns)
			}
			packet, _ := os.ReadFile(spawns[1].Packet)
			if !strings.Contains(string(packet), "## Your previous answer\n\nthis is not JSON\n") {
				t.Errorf("the repair attempt after the restart lost the answer it repairs:\n%s", packet)
			}
		})
	}
}

// B3: a held call whose repair turn was delivered goes on with a new subagent. The call has used its one repair (the App Server
// path's rule, hold.go resumeHeld), so the new subagent's turn is the repair turn again: its packet names the task once, the answer
// that is repaired and the repair instruction - never the task twice.
func TestSessionHeldRepairGoesOnAsTheRepair(t *testing.T) {
	k := newSessionKit(t)
	var mu sync.Mutex
	phase := 1
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case phase == 2:
			return orchAnswer{edit: "P:" + st.Op}
		case st.K == 1:
			return orchAnswer{text: "this is not JSON"}
		}
		return orchAnswer{fail: "usageLimitExceeded"}
	}
	got := value[[]any](t, k.h.run(e2aBuild))
	if got[0] != "held" {
		t.Fatalf("the first run %v; rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	mu.Lock()
	phase = 2
	mu.Unlock()
	k.h.cfg.Resume = &ResumeOptions{}
	got = value[[]any](t, k.h.run(e2aBuild))
	if got[0] != "completed" {
		t.Fatalf("the resumed call %v; rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	spawns := k.orch.spawned()
	last := spawns[len(spawns)-1]
	packet, _ := os.ReadFile(last.Packet)
	p := string(packet)
	if n := strings.Count(p, "build node n1"); n != 1 {
		t.Errorf("the packet names the task %d times, want once:\n%s", n, p)
	}
	_, now, _ := strings.Cut(p, "## What to do now\n\n")
	if !strings.HasPrefix(now, "Your previous answer did not satisfy the output schema") || !strings.Contains(p, "## Your previous answer\n\nthis is not JSON\n") {
		t.Errorf("the held repair does not go on as the repair of the answer it repairs:\n%s", p)
	}
}

// A4+B4: the checkpoint packet of a continuation is the record of the handle's conversation. A session continuation's subagent
// continues the conversation of the call it continues (its packet names that call's packet and answer), and a later attempt of a
// call goes on with the turns before it, so the record holds them: c3 -> c2 -> c1 reads c1's turn before c2's, and a continuation of
// a call whose repair finished on a second subagent reads the first turn before the repair.
func TestSessionCheckpointHoldsTheWholeConversation(t *testing.T) {
	t.Run("chained continuations", func(t *testing.T) {
		k := newSessionKit(t)
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer { return orchAnswer{edit: "P:" + st.Op} }
		two := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
const fix = await agent.call('fix what the review found', { resume: r, label: 'fix', schema: RECEIPT })
`
		value[[]string](t, k.h.run(two+`return [r.status, fix.status]`))
		k.h.cfg.Resume = &ResumeOptions{}
		got := value[[]string](t, k.h.run(two+`const again = await agent.call('fix the second round', { resume: fix, label: 'again', schema: RECEIPT })
return [r.status, fix.status, again.status]`))
		if strings.Join(got, ",") != "completed,completed,completed" {
			t.Fatalf("results %v", got)
		}
		cp := flowRows(k.h.journal, EvCheckpointStarted)
		if len(cp) != 1 {
			t.Fatalf("checkpoint_started rows %v", cp)
		}
		packet := rowStr(cp[0].Data, "packet")
		first, second := strings.Index(packet, "build node n1"), strings.Index(packet, "fix what the review found")
		if first < 0 || second < 0 || first > second {
			t.Errorf("the checkpoint of c3 -> c2 -> c1 does not hold c1's turn before c2's:\n%s", packet)
		}
	})
	t.Run("repair finished on a second subagent", func(t *testing.T) {
		k := newSessionKit(t)
		first := firstCallOf()
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			switch {
			case !first(st.Call):
			case st.K == 1:
				return orchAnswer{text: "this is not JSON"}
			case st.K == 2 && st.Attempt == 1:
				return orchAnswer{lose: true}
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		one := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
`
		value[string](t, k.h.run(one+`return r.status`))
		if got := eventsOf(k.rowsOf("exe")); got != "prepared,step_published,thread_started,turn_started,step_published,turn_started,session_attempt_lost,step_published,thread_started,turn_started,completed" {
			t.Fatalf("the first run's rows %s", got)
		}
		k.h.cfg.Resume = &ResumeOptions{}
		got := value[[]string](t, k.h.run(one+`const fix = await agent.call('fix what the review found', { resume: r, label: 'fix', schema: RECEIPT })
return [r.status, fix.status]`))
		if strings.Join(got, ",") != "completed,completed" {
			t.Fatalf("results %v", got)
		}
		cp := flowRows(k.h.journal, EvCheckpointStarted)
		if len(cp) != 1 {
			t.Fatalf("checkpoint_started rows %v", cp)
		}
		packet := rowStr(cp[0].Data, "packet")
		task, repair := strings.Index(packet, "[turn 1] build node n1"), strings.Index(packet, "[turn 2] Your previous answer did not satisfy")
		if task < 0 || repair < 0 || strings.Count(packet, "[turn ") != 2 {
			t.Errorf("the checkpoint of a call whose repair finished on a second subagent does not hold its first turn and then the repair:\n%s", packet)
		}
	})
	t.Run("first turn lost and run again", func(t *testing.T) {
		k := newSessionKit(t)
		first := firstCallOf()
		k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
			if first(st.Call) && st.Attempt == 1 {
				return orchAnswer{lose: true}
			}
			return orchAnswer{edit: "P:" + st.Op}
		}
		one := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
`
		value[string](t, k.h.run(one+`return r.status`))
		k.h.cfg.Resume = &ResumeOptions{}
		value[[]string](t, k.h.run(one+`const fix = await agent.call('fix what the review found', { resume: r, label: 'fix', schema: RECEIPT })
return [r.status, fix.status]`))
		cp := flowRows(k.h.journal, EvCheckpointStarted)
		if len(cp) != 1 {
			t.Fatalf("checkpoint_started rows %v", cp)
		}
		// the second attempt ran the same turn again: the record holds it once
		if packet := rowStr(cp[0].Data, "packet"); strings.Count(packet, "[turn ") != 1 || !strings.Contains(packet, "[turn 1] build node n1") {
			t.Errorf("the checkpoint holds the lost attempt's turn beside the one that ran it again:\n%s", packet)
		}
	})
	// The A4+B4 seam, its one rule: the conversation is the call's turns by turn index, each index once, the replacement in the place
	// of the turn it replaces. A held call whose turn was delivered goes on in place with a new subagent at the next k, and that turn
	// replaces the held one: the record holds the task (and the repair) once.
	for _, tc := range []struct {
		name string
		// held is the k of the turn the call is held in (usageLimitExceeded); the turns before it answer what is not JSON
		held int
		want []string
	}{
		{name: "task turn held and gone on with in place", held: 1, want: []string{"[turn 1] build node n1"}},
		{name: "repair turn held and gone on with in place", held: 2, want: []string{"[turn 1] build node n1", "[turn 2] Your previous answer did not satisfy the output schema"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newSessionKit(t)
			var mu sync.Mutex
			phase := 1
			k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case phase == 2:
				case st.K < tc.held:
					return orchAnswer{text: "this is not JSON"}
				default:
					return orchAnswer{fail: "usageLimitExceeded"}
				}
				return orchAnswer{edit: "P:" + st.Op}
			}
			one := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
`
			if got := value[[]any](t, k.h.run(one+`return [r.status, r.reason]`)); got[0] != "held" {
				t.Fatalf("the first run %v; rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			mu.Lock()
			phase = 2
			mu.Unlock()
			k.h.cfg.Resume = &ResumeOptions{}
			if got := value[string](t, k.h.run(one+`return r.status`)); got != "completed" {
				t.Fatalf("the held call gone on with %s; rows %s", got, eventsOf(k.rowsOf("exe")))
			}
			if n := len(rowsNamed(k.rowsOf("exe"), EvThreadStarted)); n != 2 {
				t.Fatalf("thread_started rows %d, want the held subagent's and the new one's; rows %s", n, eventsOf(k.rowsOf("exe")))
			}
			k.h.cfg.Resume = &ResumeOptions{}
			got := value[[]string](t, k.h.run(one+`const fix = await agent.call('fix what the review found', { resume: r, label: 'fix', schema: RECEIPT })
return [r.status, fix.status]`))
			if strings.Join(got, ",") != "completed,completed" {
				t.Fatalf("results %v", got)
			}
			cp := flowRows(k.h.journal, EvCheckpointStarted)
			if len(cp) != 1 {
				t.Fatalf("checkpoint_started rows %v", cp)
			}
			packet := rowStr(cp[0].Data, "packet")
			at := 0
			for _, w := range tc.want {
				i := strings.Index(packet[at:], w)
				if i < 0 {
					at = -1
					break
				}
				at += i + len(w)
			}
			if at < 0 || strings.Count(packet, "[turn ") != len(tc.want) || strings.Count(packet, "[answer of the call]") != 1 {
				t.Errorf("the checkpoint of a call that went on in place holds the turn it replaced beside its replacement, want %q in order and once each:\n%s", tc.want, packet)
			}
		})
	}
}

// firstCallOf says whether a step is of the first call the orchestrator saw.
func firstCallOf() func(call string) bool {
	var mu sync.Mutex
	first := ""
	return func(call string) bool {
		mu.Lock()
		defer mu.Unlock()
		if first == "" {
			first = call
		}
		return call == first
	}
}

// A5: the commit an executor call's worktree is to be at is the start commit of the allocation it works in, moved only by the
// node's crw.commit rows after that allocation: a crw.commit an earlier allocation of the node recorded (a superseded process's,
// past a suffix's boundary) is not the start of a worktree restored at another commit, so the call's HEAD at that commit is not
// recorded as moved and crw.commit does not take the earlier commit as its parent.
func TestSessionStartCommitIsTheAllocationsStart(t *testing.T) {
	alloc := func(kind, path, start string, attempt int) Event {
		return Event{RunID: "run-test", Kind: KindCall, CallID: "c_" + path, Op: "agent", Event: EvPrepared,
			Data: map[string]any{"node": "n1", "allocation": map[string]any{"kind": kind, "attempt": attempt, "path": path, "start_commit": start}}}
	}
	commit := func(c string) Event {
		return Event{RunID: "run-test", Kind: KindCall, CallID: "c_commit_" + c, Op: OpCommit, Event: EvCompleted, Data: map[string]any{"node": "n1", "commit": c}}
	}
	for _, tc := range []struct {
		name   string
		rows   []Event
		cwd    string
		pl     Placement
		start  string
		moved  bool
		headAt string
	}{
		{name: "restored after an earlier allocation's commit", rows: []Event{alloc(PlaceFresh, "/w/n1/a1", "base", 1), commit("c-old"), alloc(PlaceRestore, "/w/n1/a2", "c-boundary", 2)},
			cwd: "/w/n1/a2", pl: Placement{Kind: PlaceRestore, Attempt: 2, Path: "/w/n1/a2", Start: "c-boundary"}, start: "c-boundary", headAt: "c-boundary"},
		{name: "a commit in the allocation moves it", rows: []Event{alloc(PlaceFresh, "/w/n1/a1", "base", 1), commit("c-old"), alloc(PlaceRestore, "/w/n1/a2", "c-boundary", 2), commit("c-new")},
			cwd: "/w/n1/a2", pl: Placement{Kind: PlaceContinued, Path: "/w/n1/a2"}, start: "c-new", headAt: "c-new"},
		{name: "a fix round after the build's commit", rows: []Event{alloc(PlaceFresh, "/w/n1/a1", "base", 1), commit("c-1")},
			cwd: "/w/n1/a1", pl: Placement{Kind: PlaceContinued, Path: "/w/n1/a1"}, start: "c-1", headAt: "stray", moved: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := NewMemoryJournal()
			for _, r := range tc.rows {
				mem.Append(r)
			}
			placer := newSessPlacer()
			placer.setHead(tc.cwd, tc.headAt)
			e := &Engine{j: mem, runID: "run-test", cfg: Config{Backend: placer, Session: &SessionConfig{}}}
			pl := tc.pl
			c := &callRun{e: e, id: "c_now", plan: &callPlan{node: "n1", role: "executor", spec: RoleSpec{Family: "executor"}}, placement: &pl, cwd: tc.cwd, sess: &sessionCall{}, path: &pathState{}}
			if start, _ := c.startCommit(); start != tc.start {
				t.Errorf("startCommit %q, want %q", start, tc.start)
			}
			data := map[string]any{}
			c.sessionChecks(context.Background(), data)
			if _, moved := data["executor_head_moved"]; moved != tc.moved {
				t.Errorf("executor_head_moved %v, want moved %v", data["executor_head_moved"], tc.moved)
			}
		})
	}
}
