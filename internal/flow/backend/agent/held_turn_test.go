package agent_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1269 d1: a terminal held row does not prove that the host turn of the call stopped. An engine pause whose interrupt the
// host ignored ends the call held engine_paused while its turn still runs. A resumed run neither continues such a call nor
// replaces it (a new attempt, a kept and restored worktree) until the host shows the turn ended: it interrupts the turn and
// confirms the interruption first, and a turn that stays active keeps the call held.

const pausedScript = `const out = await parallel([
  async () => agent.call('quota-call', { agentType: 'executor', node: 'a', label: 'quota' }),
  async () => agent.call('long-call', { agentType: 'executor', node: 'b', label: 'long' }),
])
return out.map((r) => [r.status, r.reason])`

// The evaluator's trigger: parallel executor calls, one quota failure while the other host turn ignores turn/interrupt. Each
// resume while that turn is still active sends nothing on its thread, starts no new thread and restores no worktree for its
// node; the call stays held.
func TestHeldPauseIsNotContinuedOrReplacedBesideItsRunningTurn(t *testing.T) {
	e := newEnv(t, withInterruptWait(300*time.Millisecond), withOwner("proc-1"))
	gate := make(chan struct{})
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "quota-call") {
			return fakehost.TurnPlan{Gate: gate, Fail: &fakehost.Failure{Message: "quota", CodexErrorInfo: "rateLimitExceeded"}}
		}
		return fakehost.TurnPlan{Hang: true, IgnoreInterrupt: true}
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.host.WaitCount(ctx, "turn/start", 2)
		close(gate)
	}()
	if got := value[[][]string](t, e.run(pausedScript)); fmt.Sprint(got) != "[[held host_infra_rateLimitExceeded] [held engine_paused]]" {
		t.Fatalf("the first run: %v", got)
	}
	thread := str(e.row("long", engine.EvThreadStarted).Data, "thread_id")
	e.useClock()
	for round := 1; round <= 2; round++ {
		e.restart(fmt.Sprintf("proc-%d", round+1))
		e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
		before, adds := len(e.host.Requests()), e.worktreeAdds()
		res := e.resumeRun(pausedScript)
		var onThread, creations, interrupts int
		for _, r := range e.requests()[before:] {
			switch r.Method {
			case "turn/start":
				if r.Params["threadId"] == thread {
					onThread++
				}
			case "thread/start", "thread/fork":
				creations++
			case "turn/interrupt":
				if r.Params["threadId"] == thread {
					interrupts++
				}
			}
		}
		if onThread != 0 {
			t.Errorf("round %d: %d turn/start requests on thread %s while its held turn still runs", round, onThread, thread)
		}
		if creations != 0 {
			t.Errorf("round %d: %d threads were made beside the running turn", round, creations)
		}
		if n := e.worktreeAdds() - adds; n != 0 {
			t.Errorf("round %d: %d worktrees were added for a node whose turn still runs", round, n)
		}
		if interrupts == 0 {
			t.Errorf("round %d: the resume did not interrupt the running turn before deciding the call", round)
		}
		for _, ev := range e.journal.Events() {
			if ev.Kind == engine.KindFlow && (ev.Event == engine.EvWorktreePreserved || ev.Event == engine.EvWorktreeRestored) && ev.Data["node"] == "b" {
				t.Errorf("round %d: %s for node b while its held turn still runs", round, ev.Event)
			}
		}
		got := value[[][]string](t, res)
		if len(got) != 2 || got[1][0] != engine.StatusHeld {
			t.Errorf("round %d: results %v, want the long call held", round, got)
		}
		if got := kinds(e.rows("long")); countEvent(got, engine.EvPrepared) != 1 {
			t.Errorf("round %d: rows %v: no new attempt may start beside the running turn", round, got)
		}
	}
	view, _ := e.threads.Thread(thread)
	if len(view.Turns) != 1 || view.Turns[0].Status != "inProgress" {
		t.Errorf("thread %s: %+v, want the one turn still running", thread, view.Turns)
	}
}

// The same held call when an earlier position of its path changed: the resume's suffix ends the held call superseded without
// reading its thread, and the call's position runs a new attempt. The new attempt reads the thread first and, the old turn
// still running, ends held without making a thread; the next resume still knows the old attempt's turn (its superseded row
// says it was held) and keeps the call held instead of creating a thread for the new attempt.
func TestHeldPauseInASuffixKeepsItsRunningTurnInView(t *testing.T) {
	e := newEnv(t, withInterruptWait(300*time.Millisecond), withOwner("proc-1"))
	gate := make(chan struct{})
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		switch {
		case strings.Contains(r.Text, "quota-call"):
			return fakehost.TurnPlan{Gate: gate, Fail: &fakehost.Failure{Message: "quota", CodexErrorInfo: "rateLimitExceeded"}}
		case strings.Contains(r.Text, "long-call"):
			return fakehost.TurnPlan{Hang: true, IgnoreInterrupt: true}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = e.host.WaitCount(ctx, "turn/start", 3)
		close(gate)
	}()
	script := func(warm string) string {
		return `const w = await agent.call('` + warm + `', { agentType: 'explorer', label: 'warm' })
const out = await parallel([
  async () => agent.call('quota-call', { agentType: 'executor', node: 'a', label: 'quota' }),
  async () => agent.call('long-call', { agentType: 'executor', node: 'b', label: 'long' }),
])
return out.map((r) => [r.status, r.reason])`
	}
	if got := value[[][]string](t, e.run(script("warm up"))); fmt.Sprint(got) != "[[held host_infra_rateLimitExceeded] [held engine_paused]]" {
		t.Fatalf("the first run: %v", got)
	}
	thread := str(e.row("long", engine.EvThreadStarted).Data, "thread_id")
	e.useClock()
	for round := 1; round <= 2; round++ {
		e.restart(fmt.Sprintf("proc-%d", round+1))
		e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
		before, adds := len(e.host.Requests()), len(e.gitCalls())
		// the explorer's packet changed: its position, and every position after it, is the suffix
		res := e.resumeRun(script("warm up again"))
		for _, r := range e.requests()[before:] {
			if (r.Method == "turn/start" && r.Params["threadId"] == thread) || ((r.Method == "thread/start" || r.Method == "thread/fork") && strings.Contains(str(r.Params, "cwd"), "/wt/b/")) {
				t.Errorf("round %d: %s %v while the held turn of node b still runs", round, r.Method, r.Params["threadId"])
			}
		}
		for _, c := range e.gitCalls()[adds:] {
			if len(c.Args) >= 2 && c.Args[0] == "worktree" && c.Args[1] == "add" && strings.Contains(strings.Join(c.Args, " "), "/wt/b/") {
				t.Errorf("round %d: a worktree of node b was added while its held turn still runs: %v", round, c.Args)
			}
		}
		for _, ev := range e.journal.Events() {
			if ev.Kind == engine.KindFlow && (ev.Event == engine.EvWorktreePreserved || ev.Event == engine.EvWorktreeRestored) && ev.Data["node"] == "b" {
				t.Errorf("round %d: %s for node b while its held turn still runs", round, ev.Event)
			}
		}
		if got := value[[][]string](t, res); len(got) != 2 || got[1][0] != engine.StatusHeld {
			t.Errorf("round %d: results %v, want the long call held", round, got)
		}
		if n := len(e.rowsOf("long", engine.EvThreadStarted)); n != 1 {
			t.Errorf("round %d: the long call has %d threads, want its first one only; rows %v", round, n, kinds(e.rows("long")))
		}
	}
	view, _ := e.threads.Thread(thread)
	if len(view.Turns) != 1 || view.Turns[0].Status != "inProgress" {
		t.Errorf("thread %s: %+v, want the one turn still running", thread, view.Turns)
	}
}

// heldWithRunningTurn leaves the journal of a run whose one executor call is held engine_paused while its turn still runs on
// the host (a turn that honours a later interrupt): the first process wrote the call's turn_started and died, and the held
// row is the one a pause whose interrupt did not end the turn writes. It returns the call's thread and its turn_started row,
// and ends the dead process's run when the test ends.
func heldWithRunningTurn(t *testing.T, e *env, body string) (thread string, turn engine.Event) {
	t.Helper()
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 1 && strings.Contains(r.Text, "build it") {
			return fakehost.TurnPlan{Hang: true, Detached: true}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	started := make(chan engine.Event, 1)
	e.journal.onAfter(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvTurnStarted {
			select {
			case started <- ev:
			default:
			}
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	dead := make(chan struct{})
	go func() {
		defer close(dead)
		_ = e.runWith(ctx, body)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-dead:
		case <-time.After(30 * time.Second):
			t.Error("the first process's run did not end")
		}
	})
	select {
	case turn = <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the turn did not start")
	}
	e.journal.freeze()
	e.thaw()
	thread = str(e.row("a", engine.EvThreadStarted).Data, "thread_id")
	cwd := str(e.row("a", engine.EvThreadStarted).Data, "cwd")
	snap, err := e.backend.Snapshot(context.Background(), cwd)
	if err != nil {
		t.Fatal(err)
	}
	e.journal.Append(engine.Event{RunID: turn.RunID, Kind: engine.KindCall, CallID: turn.CallID, Path: turn.Path, Position: turn.Position, Op: "agent", Label: turn.Label,
		Event: engine.EvHeld, Data: map[string]any{"reason": engine.ReasonEnginePaused, "input_state": "delivered", "detail": "the interrupted turn did not end", "p_held": snap}})
	return thread, turn
}

const buildBody = `const a = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'a' }); return [a.status, a.reason]`

// A held turn that is still running when the resume starts, and that honours the interrupt, is interrupted and its end
// confirmed by thread/read before the continuation goes out on the same thread.
func TestHeldPauseInterruptsItsRunningTurnBeforeTheContinuation(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	thread, _ := heldWithRunningTurn(t, e, buildBody)
	e.restart("proc-2")
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
	e.useClock()
	before := len(e.host.Requests())
	res := e.resumeRun(buildBody)
	if got := value[[]string](t, res); got[0] != engine.StatusCompleted {
		t.Fatalf("the resumed call: %v; rows %v", got, kinds(e.rows("a")))
	}
	interrupt, confirm, cont := -1, -1, -1
	for i, r := range e.requests()[before:] {
		switch {
		case r.Method == "turn/interrupt" && r.Params["threadId"] == thread && interrupt < 0:
			interrupt = i
		case r.Method == "thread/read" && r.Params["threadId"] == thread && interrupt >= 0 && confirm < 0:
			confirm = i
		case r.Method == "turn/start" && r.Params["threadId"] == thread:
			cont = i
		case r.Method == "thread/start" || r.Method == "thread/fork":
			t.Errorf("the resume made a thread (%s)", r.Method)
		}
	}
	if interrupt < 0 || confirm < interrupt || cont < confirm {
		t.Errorf("request order: interrupt %d, confirming thread/read %d, continuation %d", interrupt, confirm, cont)
	}
	view, _ := e.threads.Thread(thread)
	if len(view.Turns) != 2 || view.Turns[0].Status != "interrupted" || view.Turns[1].Status != "completed" {
		t.Errorf("thread %s: %+v, want the held turn interrupted and the continuation completed", thread, view.Turns)
	}
	if got := kinds(e.rows("a")); countEvent(got, engine.EvPrepared) != 1 || !slices.Contains(got, engine.EvCompleted) {
		t.Errorf("rows %v: the call goes on in its own attempt", got)
	}
}

// An attempt that a resume ended held recovery_hold before it made anything (the turn of the held attempt before it could not
// be confirmed ended) starts again once the old turn is shown ended: it does not go on in place, where it has neither a thread
// nor an allocation, but runs as a new attempt after the old turn's interruption is confirmed.
func TestHeldAttemptThatWaitedForAnOldTurnStartsAgain(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	thread, turn := heldWithRunningTurn(t, e, buildBody)
	// the attempt a resume began while the old turn ran: its prepared row (no allocation: the node was stopped) and its hold
	prep := e.row("a", engine.EvPrepared)
	data := map[string]any{}
	for k, v := range prep.Data {
		if k != "allocation" {
			data[k] = v
		}
	}
	e.journal.Append(engine.Event{RunID: turn.RunID, Kind: engine.KindCall, CallID: turn.CallID, Path: turn.Path, Position: turn.Position, Op: "agent", Label: turn.Label, Event: engine.EvPrepared, Data: data})
	e.journal.Append(engine.Event{RunID: turn.RunID, Kind: engine.KindCall, CallID: turn.CallID, Path: turn.Path, Position: turn.Position, Op: "agent", Label: turn.Label,
		Event: engine.EvHeld, Data: map[string]any{"reason": engine.ReasonRecoveryHold, "input_state": "not_delivered", "detail": "a turn of the superseded suffix could not be confirmed interrupted"}})
	e.restart("proc-2")
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
	e.useClock()
	before := len(e.host.Requests())
	res := e.resumeRun(buildBody)
	if got := value[[]string](t, res); got[0] != engine.StatusCompleted {
		t.Fatalf("the resumed call: %v; rows %v", got, kinds(e.rows("a")))
	}
	interrupt, confirm, start := -1, -1, -1
	for i, r := range e.requests()[before:] {
		switch {
		case r.Method == "turn/interrupt" && r.Params["threadId"] == thread && interrupt < 0:
			interrupt = i
		case r.Method == "thread/read" && r.Params["threadId"] == thread && interrupt >= 0 && confirm < 0:
			confirm = i
		case r.Method == "thread/start" && start < 0:
			start = i
		}
	}
	if interrupt < 0 || confirm < interrupt || start < confirm {
		t.Errorf("request order: interrupt %d, confirming thread/read %d, the new attempt's thread/start %d", interrupt, confirm, start)
	}
	if view, _ := e.threads.Thread(thread); len(view.Turns) != 1 || view.Turns[0].Status != "interrupted" {
		t.Errorf("thread %s: %+v, want the old turn interrupted", thread, view.Turns)
	}
	if got := kinds(e.rows("a")); countEvent(got, engine.EvPrepared) != 3 {
		t.Errorf("rows %v: the call runs a new attempt", got)
	}
}

func countEvent(rows []string, event string) int {
	n := 0
	for _, r := range rows {
		if r == event {
			n++
		}
	}
	return n
}
