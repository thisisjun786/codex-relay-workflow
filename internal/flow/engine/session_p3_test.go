package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-1392: the session-engine P3s that CRW-1367, CRW-1362 and CRW-1278 left: the adoption's lost path, the continuation's pair
// rules, the step bus's permanent failure and the tokens of a held call.

// resumeWithin resumes the e2aKit's run like resume, with setup applied to the resumed process's configuration first, and gives the
// process d to end: a run that does not end in time is cancelled and its result returned as it is.
func (k *e2aKit) resumeWithin(body string, d time.Duration, setup func()) *Result {
	k.t.Helper()
	k.h.cfg.Journal = k.mem
	k.cfg.Bus = k.bus
	k.cfg.Host = k.host
	k.h.cfg.Control = nil
	k.h.cfg.Resume = &ResumeOptions{}
	k.h.cfg.SpentBefore = RunSpent(k.mem.Events(), k.h.cfg.RunID).Total
	if setup != nil {
		setup()
	}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	res, err := Run(ctx, k.h.load(body), k.h.cfg)
	if err != nil {
		k.t.Fatal(err)
	}
	return res
}

// onRow is a journal that calls f, once, right after it stored the first call row named event.
type onRow struct {
	Journal
	event string
	f     func(Event)
	once  sync.Once
}

func (j *onRow) Append(e Event) Event {
	out := j.Journal.Append(e)
	if e.Kind == KindCall && e.Event == j.event {
		j.once.Do(func() { j.f(out) })
	}
	return out
}

func (j *onRow) Err() error {
	if f, ok := j.Journal.(FailingJournal); ok {
		return f.Err()
	}
	return nil
}

// goneClock is a host that notes when it first showed agent-1 gone (unknown, or a thread status hostGone names).
type goneClock struct {
	*fakeSessionHost
	mu    sync.Mutex
	first time.Time
}

func (h *goneClock) ReadAgent(ctx context.Context, id string) (SessionAgent, error) {
	ag, err := h.fakeSessionHost.ReadAgent(ctx, id)
	if id == "agent-1" && (errors.Is(err, ErrSessionAgentNotFound) || err == nil && hostGone(ag.Status)) {
		h.mu.Lock()
		if h.first.IsZero() {
			h.first = time.Now()
		}
		h.mu.Unlock()
	}
	return ag, err
}

// The adoption of a recorded subagent (a process died after thread_started) loses the attempt when the host shows the subagent gone
// for LostAfter - unknown to it, or a thread it shows notLoaded or systemError whose marker turn it cannot list - as the harvest
// judges a bound one, and not before: the lost row comes LostAfter after the first read that showed it gone, and records the status
// the host showed. The next attempt spawns and completes the call.
func TestSessionAdoptLosesAGoneSubagentAfterLostAfter(t *testing.T) {
	for _, status := range []string{"notFound", "notLoaded", "systemError"} {
		t.Run(status, func(t *testing.T) {
			k := newE2aKit(t, deathAt(EvThreadStarted))
			k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
				if k.phaseNow() == 1 {
					return orchAnswer{hang: true}
				}
				return orchAnswer{edit: "P:" + st.Op}
			}
			k.die(e2aBuild)
			k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) {
				if status == "notFound" {
					a.gone = true
					return
				}
				// the host still reads the thread, and lists none of its turns
				a.Status, a.turns = status, nil
			})
			clock := &goneClock{fakeSessionHost: k.host}
			var lostAt time.Time
			res := k.resumeWithin(e2aBuild, 5*time.Second, func() {
				k.cfg.Host = clock
				k.h.cfg.Journal = &onRow{Journal: k.mem, event: EvSessionAttemptLost, f: func(Event) { lostAt = time.Now() }}
			})
			rows := k.rowsOf("exe")
			if res.Status != RunCompleted {
				t.Fatalf("the resumed run %s %s; rows %s", res.Status, res.Error, eventsOf(rows))
			}
			if got := value[[]any](t, res); got[0] != "completed" {
				t.Fatalf("the resumed call %v; rows %s", got, eventsOf(rows))
			}
			lost := rowsNamed(rows, EvSessionAttemptLost)
			if len(lost) != 1 || rowStr(lost[0].Data, "host_status") != status || rowInt(lost[0].Data, "attempt") != 1 {
				t.Fatalf("session_attempt_lost %v, want attempt 1 lost with host_status %s", lost, status)
			}
			clock.mu.Lock()
			first := clock.first
			clock.mu.Unlock()
			if first.IsZero() || lostAt.Sub(first) < k.cfg.LostAfter {
				t.Errorf("the attempt was lost %s after the host first showed its subagent gone, want at least LostAfter %s", lostAt.Sub(first), k.cfg.LostAfter)
			}
		})
	}
}

// flakyReads is a host whose reads of agent-1 fail every third time, as a control socket that does not answer for a moment.
type flakyReads struct {
	*goneClock
	mu sync.Mutex
	n  int
}

func (h *flakyReads) ReadAgent(ctx context.Context, id string) (SessionAgent, error) {
	if id == "agent-1" {
		h.mu.Lock()
		h.n++
		fail := h.n%3 == 0
		h.mu.Unlock()
		if fail {
			return SessionAgent{}, errors.New("the control socket is not answering")
		}
	}
	return h.goneClock.ReadAgent(ctx, id)
}

// A read of the recorded subagent the host cannot answer judges nothing, in the adoption as in the harvest: the time the subagent
// has been gone stays as it is, so a subagent the host shows gone between failed reads is lost LostAfter after it was first shown
// gone, and the next attempt completes the call.
func TestSessionAdoptKeepsTheGoneTimeOverAFailedRead(t *testing.T) {
	k := newE2aKit(t, deathAt(EvThreadStarted))
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if k.phaseNow() == 1 {
			return orchAnswer{hang: true}
		}
		return orchAnswer{edit: "P:" + st.Op}
	}
	k.die(e2aBuild)
	k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) { a.gone = true })
	host := &flakyReads{goneClock: &goneClock{fakeSessionHost: k.host}}
	var lostAt time.Time
	res := k.resumeWithin(e2aBuild, 5*time.Second, func() {
		k.cfg.Host = host
		k.h.cfg.Journal = &onRow{Journal: k.mem, event: EvSessionAttemptLost, f: func(Event) { lostAt = time.Now() }}
	})
	rows := k.rowsOf("exe")
	if res.Status != RunCompleted {
		t.Fatalf("the resumed run %s %s: the gone subagent was never lost; rows %s", res.Status, res.Error, eventsOf(rows))
	}
	if got := value[[]any](t, res); got[0] != "completed" {
		t.Fatalf("the resumed call %v; rows %s", got, eventsOf(rows))
	}
	lost := rowsNamed(rows, EvSessionAttemptLost)
	if len(lost) != 1 || rowStr(lost[0].Data, "host_status") != "notFound" || rowInt(lost[0].Data, "attempt") != 1 {
		t.Fatalf("session_attempt_lost %v, want attempt 1 lost with host_status notFound", lost)
	}
	host.mu.Lock()
	failed := host.n / 3
	host.mu.Unlock()
	host.goneClock.mu.Lock()
	first := host.goneClock.first
	host.goneClock.mu.Unlock()
	if failed == 0 || first.IsZero() || lostAt.Sub(first) < k.cfg.LostAfter {
		t.Errorf("the attempt was lost %s after the host first showed its subagent gone (%d failed reads), want at least LostAfter %s", lostAt.Sub(first), failed, k.cfg.LostAfter)
	}
}

// The adoption's lost path passes the gate before it binds the next attempt, as the harvest's does: a run that is paused (or a node
// that was stopped) when the recorded subagent is lost publishes no step for the next attempt and holds the call.
func TestSessionAdoptLossPassesTheGate(t *testing.T) {
	k := newE2aKit(t, deathAt(EvThreadStarted))
	k.orch.answer = func(st Step, _ string, _ int) orchAnswer {
		if k.phaseNow() == 1 {
			return orchAnswer{hang: true}
		}
		return orchAnswer{edit: "P:" + st.Op}
	}
	k.die(e2aBuild)
	k.host.set("agent-1", func(a *fakeAgent, _ *SessionTurn) { a.gone = true })
	ctl := &Control{}
	res := k.resumeWithin(e2aBuild, 5*time.Second, func() {
		k.h.cfg.Control = ctl
		// the operator's pause comes the moment the attempt is lost
		k.h.cfg.Journal = &onRow{Journal: k.mem, event: EvSessionAttemptLost, f: func(Event) { ctl.Pause("operator", "test") }}
	})
	rows := k.rowsOf("exe")
	if got := value[[]any](t, res); fmt.Sprint(got) != "[held engine_paused]" {
		t.Fatalf("the resumed call %v; rows %s", got, eventsOf(rows))
	}
	for _, r := range rowsNamed(rows, EvStepPublished) {
		if rowInt(r.Data, "attempt") != 1 {
			t.Errorf("a step of attempt %d was published while the run was paused: rows %s", rowInt(r.Data, "attempt"), eventsOf(rows))
		}
	}
}

// CRW-1367 P3: the App Server path compares the whole pair of a continuation's thread with the role's pair; only a session run
// compares the decided parts. A thread the host read back on another effort than the role's undecided one is refused, and its
// continuation is refused pair_refused too.
func TestAppServerContinuationComparesTheWholePair(t *testing.T) {
	h := newHarness(t)
	h.cfg.Policy = explorerOn(Pair{"claude-fake-haiku", ""})
	h.fake.StartHook = func(ThreadSpec) (ThreadInfo, error) {
		return ThreadInfo{Pair: Pair{"claude-fake-haiku", "high"}}, nil
	}
	got := value[[]any](t, h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
const more = await agent.call('look again', { resume: r, label: 'more' })
return [r.status, r.reason, more.status, more.reason]`))
	if fmt.Sprint(got) != "[refused pair_refused refused pair_refused]" {
		t.Fatalf("results %v; rows of the continuation %s", got, eventsOf(h.callRows("more")))
	}
	rows := h.callRows("more")
	if want := "the thread reads back claude-fake-haiku/high, the role's pair is claude-fake-haiku/"; eventsOf(rows) != "prepared,refused" || rowStr(rows[1].Data, "detail") != want {
		t.Errorf("continuation rows %s %v, want refused %q", eventsOf(rows), rows[len(rows)-1].Data, want)
	}
}

// The App Server path keeps its rule: a thread is bound only on the role's whole pair, so an explicit effort on a continuation of a
// role whose effort is undecided names an effort the thread does not run, and is refused.
func TestAppServerContinuationEffortOfAnUndecidedRoleIsRefused(t *testing.T) {
	h := newHarness(t)
	h.cfg.Policy = explorerOn(Pair{"claude-fake-haiku", ""})
	got := value[[]any](t, h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
const more = await agent.call('look again', { resume: r, label: 'more', effort: 'high' })
return [r.status, more.status, more.reason]`))
	if fmt.Sprint(got) != "[completed refused pair_refused]" {
		t.Fatalf("results %v; continuation rows %s", got, eventsOf(h.callRows("more")))
	}
}

// CRW-1367 P3: an explicit effort on the continuation of a role whose effort is undecided is compared with the effort the call's
// subagent ran (the decided part of the thread's pair), not with the empty one: the effort the host applied continues, another is
// refused pair_refused.
func TestSessionContinuationEffortOfAnUndecidedRole(t *testing.T) {
	for _, tc := range []struct {
		effort, status, reason string
	}{
		{"high", "completed", ""},
		{"xhigh", "refused", "pair_refused"},
	} {
		t.Run(tc.effort, func(t *testing.T) {
			k := newSessionKit(t)
			k.h.cfg.Policy = explorerOn(Pair{"claude-fake-haiku", ""})
			applied := Pair{"claude-fake-haiku", "high"}
			k.orch.answer = func(Step, string, int) orchAnswer { return orchAnswer{pair: &applied} }
			got := value[[]any](t, k.h.run(`const r = await agent.call('look', { agentType: 'explorer', label: 'x' })
const more = await agent.call('look again', { resume: r, label: 'more', effort: '`+tc.effort+`' })
return [r.status, more.status, more.reason]`))
			var reason any
			if tc.reason != "" {
				reason = tc.reason
			}
			if fmt.Sprint(got) != fmt.Sprint([]any{"completed", tc.status, reason}) {
				t.Fatalf("results %v; continuation rows %s %v", got, eventsOf(k.rowsOf("more")), k.rowsOf("more"))
			}
			if tc.reason != "" {
				rows := k.rowsOf("more")
				if detail := rowStr(rows[len(rows)-1].Data, "detail"); detail != "effort xhigh is not the thread's effort high" {
					t.Errorf("refusal detail %q", detail)
				}
			}
		})
	}
}

// CRW-1392: a step the bus cannot write for good (EACCES on the steps directory) is still written again at the poll and never ends
// the call (fail open), and the record shows how long: step_unwritten is written at the first failure and again at the 10th, the
// 100th, ... failure, each with the count and the first and the last time, and a closing row whenever a failure row was written:
// when the step is written (after any number of failures, a power of ten among them) or the turn ends.
func TestSessionStepBusPermanentFailureIsCounted(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fails int
		want  string
	}{
		{name: "written at last", fails: 25, want: "1/<nil>,10/<nil>,25/true"},
		{name: "written after exactly ten failures", fails: 10, want: "1/<nil>,10/<nil>,10/true"},
		{name: "written after one failure", fails: 1, want: "1/<nil>,1/true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			k := newSessionKit(t)
			k.cfg.Bus = &failingBus{FileStepBus: k.bus, fails: tc.fails}
			got := value[string](t, k.h.run(`const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return r.status`))
			if got != "completed" {
				t.Fatalf("status %s", got)
			}
			warn := flowRows(k.h.journal, EvStepUnwritten)
			var counts []string
			for _, w := range warn {
				counts = append(counts, fmt.Sprintf("%d/%v", rowInt(w.Data, "failures"), w.Data["written"]))
				if rowStr(w.Data, "first_at") == "" || rowStr(w.Data, "last_at") == "" || rowStr(w.Data, "input_state") != "not_delivered" {
					t.Errorf("step_unwritten %v names no first and last time", w.Data)
				}
			}
			if strings.Join(counts, ",") != tc.want {
				t.Errorf("step_unwritten counts %v, want %s: the failure rows and the closing row of %d failures, written", counts, tc.want, tc.fails)
			}
		})
	}
	t.Run("never written", func(t *testing.T) {
		k := newSessionKit(t)
		bus := &failingBus{FileStepBus: k.bus, fails: 1 << 30}
		k.cfg.Bus = bus
		ctl := &Control{}
		k.h.cfg.Control = ctl
		go func() {
			for i := 0; i < 10000; i++ {
				bus.mu.Lock()
				left := bus.fails
				bus.mu.Unlock()
				if 1<<30-left >= 150 {
					ctl.Pause("operator", "test")
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
		got := value[[]any](t, k.h.run(`const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.reason]`))
		if fmt.Sprint(got) != "[held engine_paused]" {
			t.Fatalf("result %v", got)
		}
		warn := flowRows(k.h.journal, EvStepUnwritten)
		if len(warn) != 4 || rowInt(warn[1].Data, "failures") != 10 || rowInt(warn[2].Data, "failures") != 100 || rowInt(warn[3].Data, "failures") < 150 || warn[3].Data["written"] != false {
			var all []any
			for _, w := range warn {
				all = append(all, w.Data)
			}
			t.Errorf("step_unwritten rows %v, want the 1st, 10th and 100th failure and a closing row of the turn's end, not written", all)
		}
		for _, e := range k.h.journal.Events() {
			if e.Event == EvOutcomeUnknown {
				t.Errorf("a step that was never written ended the call unknown")
			}
		}
	})
}

// CRW-1392 (the b2 P2 of CRW-1278): a held session call that goes on in place counts its tokens once. The held row carries what the
// process that held it spent, the call goes on with a new subagent whose rollout counts from zero, and the completed row carries only
// what the resumed process spent: the run's figure is the sum of both subagents' rollouts, never the held one's twice.
func TestSessionHeldCallCountsItsTokensOnce(t *testing.T) {
	k := newSessionKit(t)
	var mu sync.Mutex
	phase := 1
	k.host.tokens["/rollout/agent-1"] = Tokens{Input: 10, Output: 5, Total: 15}
	// the owner names its subagents by the steps it took: the repair input was the second, so the new subagent is agent-3
	k.host.tokens["/rollout/agent-3"] = Tokens{Input: 15, Output: 5, Total: 20}
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
	body := `const r = await agent.call('build node n1', { agentType: 'executor', label: 'exe', node: 'n1', schema: RECEIPT })
return [r.status, r.meta.tokens.total]`
	if got := value[[]any](t, k.h.run(body)); fmt.Sprint(got) != "[held 15]" {
		t.Fatalf("the first run %v; rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	mu.Lock()
	phase = 2
	mu.Unlock()
	k.h.cfg.Resume = &ResumeOptions{}
	k.h.cfg.SpentBefore = RunSpent(k.h.journal.Events(), k.h.cfg.RunID).Total
	res := k.h.run(body)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[completed 20]" {
		t.Fatalf("the resumed call %v; rows %s", got, eventsOf(k.rowsOf("exe")))
	}
	if spent := RunSpent(k.h.journal.Events(), k.h.cfg.RunID).Total; spent != 35 || res.Spent != 35 {
		t.Errorf("the run spent %d by the journal and %d by the resumed process, want 35 (15 before the hold, 20 after)", spent, res.Spent)
	}
}
