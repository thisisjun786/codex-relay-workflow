package agent_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/backend/agent"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

var hostCannotAnswer = fakehost.Reply{ErrorObject: map[string]any{"code": -32603, "message": "the host cannot answer"}}

// An outcome that reconcile left unknown is established by a later pass that reads the host: the call is reopened
// (recovery_cleared), the hold ends and the call goes on - for a turn_started that was recorded as well as for a creation
// that is proven absent.
func TestLaterReconcileClearsAnUnknownHold(t *testing.T) {
	t.Run("recorded turn", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.killAfter(engine.EvTurnStarted)
		a := calls(e.run(buildScript))["a"]
		e.thaw()
		e.restart("proc-2")
		e.host.Script("thread/turns/list", hostCannotAnswer)
		res := e.resumeRun(buildCall + "return [r.status, r.reason]")
		if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
			t.Fatalf("%v", got)
		}
		e.expectUnknown("a", "n1")
		res = e.resumeRun(buildScript)
		if c := calls(res)["a"]; c.Status != engine.StatusCompleted {
			t.Fatalf("a: %s; rows %v", callStatus(c), kinds(e.rows("a")))
		}
		if len(engine.RecoveryHolds(e.journal, e.runID)) != 0 {
			t.Errorf("the hold did not end: %v", engine.RecoveryHolds(e.journal, e.runID))
		}
		if len(e.rowsOf("a", engine.EvRecoveryCleared)) != 1 {
			t.Errorf("rows %v: the cleared hold is recorded", kinds(e.rows("a")))
		}
		if n := e.markerCount(a.CallID + "/t1"); n != 1 {
			t.Errorf("%d turn/start requests carried the operation", n)
		}
		after := e.resumeRun(buildCall + "const b = await agent.call('again', { agentType: 'executor', node: 'n1', label: 'b' })\nreturn [r.status, b.status]")
		if got := value[[]string](t, after); got[0] != "completed" || got[1] != "completed" {
			t.Errorf("a later resume: %v", got)
		}
	})
	t.Run("absent creation", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.killAfter(engine.EvThreadRequested)
		a := calls(e.run(buildScript))["a"]
		e.thaw()
		e.restart("proc-2")
		e.host.Script("thread/list", hostCannotAnswer)
		res := e.resumeRun(buildCall + "return [r.status, r.reason]")
		if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
			t.Fatalf("%v", got)
		}
		e.expectUnknown("a", "n1")
		res = e.resumeRun(buildScript)
		if c := calls(res)["a"]; c.Status != engine.StatusCompleted {
			t.Fatalf("a: %s; rows %v", callStatus(c), kinds(e.rows("a")))
		}
		if len(engine.RecoveryHolds(e.journal, e.runID)) != 0 {
			t.Errorf("the hold did not end: %v", engine.RecoveryHolds(e.journal, e.runID))
		}
		if n := e.count("thread/start"); n != 1 {
			t.Errorf("%d thread/start requests, want the one sent after the proof", n)
		}
		if n := e.markerCount(a.CallID + "/t1"); n != 1 {
			t.Errorf("%d turn/start requests carried the operation", n)
		}
	})
}

// A call the resumed script does not run is not a position that reconcile's record will carry on: a new attempt of the same
// node first interrupts the live turn of the old attempt and ends it superseded, whatever reconcile found for it.
func TestNewAttemptSupersedesAnUnclaimedRunningCall(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "first attempt") {
			return fakehost.TurnPlan{Hang: true, Detached: true}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	started := make(chan struct{})
	var once sync.Once
	e.journal.onAfter(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvTurnStarted {
			once.Do(func() { close(started) })
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dead := make(chan *engine.Result, 1)
	go func() {
		dead <- e.runWith(ctx, `const r = await parallel([
  async () => agent.call('first attempt', { agentType: 'executor', node: 'n1', label: 'old' }),
  async () => 0,
])
return r.length`)
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first attempt's turn did not start")
	}
	e.journal.freeze()
	e.thaw()
	e.restart("proc-2")
	firstThread := e.threads.All()[0].ID
	// the resumed script no longer runs the old position; a new call on the same node starts another attempt
	res := e.resumeRun(`const r = await parallel([
  async () => 0,
  async () => agent.call('second attempt', { agentType: 'executor', node: 'n1', label: 'new' }),
])
return r.length`)
	if c := calls(res)["new"]; c.Status != engine.StatusCompleted {
		t.Fatalf("the new attempt: %s; rows %v", callStatus(c), kinds(e.rows("new")))
	}
	if got := e.threads.Interrupts(); !slices.Equal(got, []string{firstThread}) {
		t.Errorf("interrupted threads %v, want [%s]: the new attempt started beside a live turn", got, firstThread)
	}
	if rows := e.rowsOf("old", engine.EvSuperseded); len(rows) != 1 || rows[0].Data["reason"] != engine.ReasonSupersededByAttempt {
		t.Errorf("the old attempt's rows %v", kinds(e.rows("old")))
	}
	cancel()
	select {
	case <-dead:
	case <-time.After(30 * time.Second):
		t.Error("the first process's run did not end")
	}
}

// recreate replaces the backend by a new one in the same process, on the same connection: the owner is the same.
func (e *env) recreate() {
	e.t.Helper()
	b, err := agent.New(agent.Config{Client: e.client, Allocator: e.alloc, Threads: e.owned, PollEvery: e.cfg.poll, InterruptWait: e.cfg.interruptWait,
		Owner: e.backend.Owner(), OwnerAlive: e.ownerAlive})
	if err != nil {
		e.t.Fatal(err)
	}
	e.backend = b
}

// The record of which creation requests were written belongs to the process, not to one backend value: a backend made again
// in the same process, whose predecessor's thread/start is still unanswered on an open connection, waits and never sends
// the creation again; and one whose predecessor's connection closed holds the grace.
func TestRecreatedBackendKeepsTheCreationEvidence(t *testing.T) {
	t.Run("answer pending on an open connection", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.client.BoundAckOn("thread/start", 150*time.Millisecond)
		e.host.Script("thread/start", fakehost.Reply{Delay: 60 * time.Second})
		if a := calls(e.run(buildScript))["a"]; a.Status != engine.StatusUnknown {
			t.Fatalf("the first run: %s", callStatus(a))
		}
		e.client.BoundAckOn("thread/start", 0)
		e.recreate()
		res := e.resumeRun(buildCall + "return [r.status]")
		if got := value[[]string](t, res); got[0] != "unknown" {
			t.Fatalf("%v", got)
		}
		if n := e.count("thread/start"); n != 1 {
			t.Errorf("%d thread/start requests: the answer of the first is still pending", n)
		}
	})
	t.Run("connection closed", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		clk := e.useClock()
		e.host.Script("thread/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: websocket.StatusInternalError, Reason: "gone"}})
		a := calls(e.run(buildScript))["a"]
		if a.Status != engine.StatusUnknown {
			t.Fatalf("the first run: %s", callStatus(a))
		}
		e.recreate()
		res := e.resumeRun(buildScript)
		e.expectCompletedOnce(res, "a", a.CallID+"/t1")
		if got := clk.waited(); got < 2*time.Minute {
			t.Errorf("waited %v, the creation grace is 2 minutes", got)
		}
	})
}
