package engine

import (
	"context"
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
