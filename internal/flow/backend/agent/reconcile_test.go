package agent_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The scenarios of F05: a creation or a turn request that got no answer, or a run process that died after an intent row or
// after a send, is looked up on the fake host by the fields the 0.154 host returns, and what is found decides the call. A
// scenario ends with the host holding exactly one turn per turn operation (or none, and the node in recovery_hold), and no
// creation was sent again without the proof that the first request had finished.

const (
	executorModel  = "claude-fake-haiku"
	executorEffort = "xhigh"
)

// fakeClock is the clock of the engine and of the fake host in a scenario; waiting advances it.
type fakeClock struct {
	mu    sync.Mutex
	t     time.Time
	waits []time.Duration
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// Wait is the Wait of a resumed run: it advances the clock by what it was asked to wait for.
func (c *fakeClock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.waits = append(c.waits, d)
	c.mu.Unlock()
	return ctx.Err()
}

func (c *fakeClock) waited() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	var total time.Duration
	for _, d := range c.waits {
		total += d
	}
	return total
}

// useClock puts the engine and the fake host on one fake clock and sets the timings of resumed runs.
func (e *env) useClock() *fakeClock {
	clk := &fakeClock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
	e.now = clk.Now
	e.threads.Clock(clk.Now)
	e.opts = &engine.ResumeOptions{Wait: clk.Wait, OpenWait: 3 * time.Second, Poll: time.Second}
	return clk
}

// resumeRun runs the script as a resume of the journal.
func (e *env) resumeRun(body string) *engine.Result {
	e.t.Helper()
	e.resume = e.opts
	defer func() { e.resume = nil }()
	return e.run(body)
}

// reconcileConfig is the configuration of a reconcile pass the test calls itself.
func (e *env) reconcileConfig(clk *fakeClock) engine.ReconcileConfig {
	return engine.ReconcileConfig{RunID: e.runID, Host: e.backend, Now: clk.Now, Wait: clk.Wait, OpenWait: 3 * time.Second, Poll: time.Second}
}

// thaw gives the journal a new writer after the process that wrote it was killed: the dead process's writer stays frozen.
func (e *env) thaw() {
	e.journal = &hookJournal{MemoryJournal: e.journal.MemoryJournal}
	e.killable = nil
}

// kill ends the process: it writes no more rows and sends no more requests.
func (e *env) kill() {
	e.journal.freeze()
	e.killable.dead.Store(true)
}

// killBefore kills the process when it is about to write the row `event`: that row and every later row are lost.
func (e *env) killBefore(event string) {
	e.killable = &killable{Backend: e.backend}
	e.journal.onBefore(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Event == event {
			e.kill()
		}
	})
}

// killAfter kills the process right after it wrote the row `event`: nothing it does next reaches the host or the journal.
func (e *env) killAfter(event string) {
	e.killable = &killable{Backend: e.backend}
	e.journal.onAfter(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Event == event {
			e.kill()
		}
	})
}

// markerCount counts the turn/start requests that carried the operation op as their clientUserMessageId.
func (e *env) markerCount(op string) int {
	n := 0
	for _, r := range e.requests("turn/start") {
		if r.Params["clientUserMessageId"] == op {
			n++
		}
	}
	return n
}

func (e *env) count(method string) int { return e.host.Count(method) }

// held reports whether the node holds a call with an unknown outcome in the journal.
func (e *env) held(node string) bool { return len(engine.RecoveryHolds(e.journal, e.runID)[node]) > 0 }

// lostAck makes the first request of the method lose its answer.
func (e *env) lostAck(method string, nth int) {
	e.threads.DropAck(method, func(n int, _ map[string]any) bool { return n == nth })
}

func hasEvent(rows []engine.Event, event string) bool {
	for _, r := range rows {
		if r.Event == event {
			return true
		}
	}
	return false
}

// rowsOf returns the rows of one call (by label) with the event name.
func (e *env) rowsOf(label, event string) []engine.Event {
	var out []engine.Event
	for _, r := range e.rows(label) {
		if r.Event == event {
			out = append(out, r)
		}
	}
	return out
}

func (e *env) flows(event string) []engine.Event { return e.flowRows(event) }

func callStatus(c engine.CallResult) string { return c.Status + "/" + c.Reason }

// buildScript is one executor call on node n1.
const buildCall = `const r = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'a' })
`

const buildScript = buildCall + `return [r.status, r.reason]`

// expectCompletedOnce checks that a recovered call completed, its turn reached the host once and nothing was held.
func (e *env) expectCompletedOnce(res *engine.Result, label string, ops ...string) {
	e.t.Helper()
	c := calls(res)[label]
	if c.Status != engine.StatusCompleted {
		e.t.Fatalf("%s: %s, want completed; rows %v", label, callStatus(c), kinds(e.rows(label)))
	}
	for _, op := range ops {
		if n := e.markerCount(op); n != 1 {
			e.t.Errorf("%d turn/start requests carried %s, want exactly one", n, op)
		}
	}
	if hasEvent(e.rows(label), engine.EvOutcomeUnknown) {
		e.t.Errorf("%s: outcome_unknown among %v", label, kinds(e.rows(label)))
	}
	if len(engine.RecoveryHolds(e.journal, e.runID)) != 0 {
		e.t.Errorf("recovery_hold: %v", engine.RecoveryHolds(e.journal, e.runID))
	}
}

// expectUnknown checks the state a call is left in when its outcome cannot be established: outcome_unknown, the
// dispatch_unknown escalation naming the node, the node in recovery_hold.
func (e *env) expectUnknown(label, node string) engine.Event {
	e.t.Helper()
	rows := e.rowsOf(label, engine.EvOutcomeUnknown)
	if len(rows) != 1 || rows[0].Data["reason"] != engine.ReasonDispatchUnknown {
		e.t.Fatalf("%s: outcome_unknown rows %v among %v", label, rows, kinds(e.rows(label)))
	}
	var escalation bool
	for _, f := range e.flows("escalation") {
		escalation = escalation || (f.Data["reason"] == engine.ReasonDispatchUnknown && f.Data["node"] == node && f.Data["call_id"] == rows[0].CallID)
	}
	if !escalation {
		e.t.Errorf("%s: no dispatch_unknown escalation for node %s: %v", label, node, e.flows("escalation"))
	}
	if !e.held(node) {
		e.t.Errorf("node %s is not in recovery_hold", node)
	}
	return rows[0]
}

// A thread/start whose answer was lost made a thread at the call's own cwd; reconcile finds it by that cwd (and the source
// the backend connects as), sees it holds no turn, adopts it and the call sends its first turn once.
func TestAckLostThreadStartAdoptsTurnlessByCwd(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.lostAck("thread/start", 1)
	first := e.run(buildScript)
	a := calls(first)["a"]
	if a.Status != engine.StatusUnknown || a.Reason != engine.ReasonDispatchUnknown {
		t.Fatalf("the first run: %s", callStatus(a))
	}
	req := e.row("a", engine.EvThreadRequested).Data
	if req["send"] != 1 || req["owner"] != "proc-1" || req["at"] == nil {
		t.Errorf("thread_requested %v", req)
	}
	cwd := e.wt("n1", 1, 0)
	if req["cwd"] != cwd {
		t.Fatalf("the call's cwd is %v, want %s", req["cwd"], cwd)
	}
	if last := kinds(e.rows("a")); last[len(last)-1] != engine.EvThreadRequested {
		t.Fatalf("rows %v", last)
	}
	e.restart("proc-2")
	res := e.resumeRun(buildScript)
	e.expectCompletedOnce(res, "a", a.CallID+"/t1")
	if got := kinds(e.rows("a")); !slices.Equal(got, []string{"prepared", "thread_requested", "thread_started", "turn_requested", "turn_started", "completed"}) {
		t.Errorf("rows %v", got)
	}
	started := e.row("a", engine.EvThreadStarted).Data
	if started["recovered"] != true || started["cwd"] != cwd || started["evidence"] == nil {
		t.Errorf("thread_started %v", started)
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests: the thread was found, not created again", n)
	}
	lists := e.requests("thread/list")
	if len(lists) == 0 || lists[0].Params["cwd"] != cwd || fmt.Sprint(lists[0].Params["sourceKinds"]) != "[appServer]" {
		t.Errorf("thread/list %v", lists)
	}
	if e.count("thread/turns/list") == 0 {
		t.Errorf("the candidate's history was not read")
	}
	if n := e.count("turn/start"); n != 1 {
		t.Errorf("%d turn/start requests", n)
	}
}

// restoreFork builds the fork scenario: the call "third" continues "mid" into a restored worktree, so it forks the thread
// through mid's last turn. The thread holds three turns by then (first's turn and repair, mid's turn).
func (e *env) restoreFork() (restored, script string) {
	e.t.Helper()
	restored = e.restoreAfter("mid", 2, 1)
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 1 {
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	script = `const S = ` + verdictSchema + `
const first = await agent.call('one', { agentType: 'executor', node: 'n1', schema: S, label: 'first' })
const mid = await agent.call('two', { resume: first, schema: S, label: 'mid' })
const third = await agent.call('three', { resume: mid, schema: S, label: 'third' })
return [first.status, mid.status, third.status]`
	return restored, script
}

// forkOf finds the fork thread the lost answer left on the host (the thread the original ThreadView names as its source).
func (e *env) forkThreads(source string) []fakehost.ThreadView {
	var out []fakehost.ThreadView
	for _, th := range e.threads.All() {
		if th.ForkedFromID == source {
			out = append(out, th)
		}
	}
	return out
}

// A thread/fork whose answer was lost made a fork at the call's private cwd that holds the copied prefix (three turns up to
// through_turn_id, the official 0.154 fork shape). Reconcile finds it by cwd and forkedFromId, reads its history with
// thread/turns/list, sees it is exactly the recorded prefix and adopts it: no outcome_unknown, no second thread/fork.
func TestAckLostForkAdoptsByCwdAndForkedFrom(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	restored, script := e.restoreFork()
	e.lostAck("thread/fork", 1)
	first := e.run(script)
	by := calls(first)
	if by["first"].Status != "completed" || by["mid"].Status != "completed" || by["third"].Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s %s %s", callStatus(by["first"]), callStatus(by["mid"]), callStatus(by["third"]))
	}
	req := e.row("third", engine.EvForkRequested).Data
	prefix, _ := req["prefix"].([]map[string]any)
	if len(prefix) != 3 || req["cwd"] != restored || req["send"] != 1 {
		t.Fatalf("fork_requested %v", req)
	}
	source := by["first"].Meta.ThreadID
	forks := e.forkThreads(source)
	if len(forks) != 1 || len(forks[0].Turns) != 3 || forks[0].Cwd != restored {
		t.Fatalf("the host holds %+v after the lost answer", forks)
	}
	e.restart("proc-2")
	res := e.resumeRun(script)
	third := calls(res)["third"]
	if third.Status != engine.StatusCompleted || third.Meta.ThreadID != forks[0].ID {
		t.Fatalf("third: %s on %s, want completed on the found fork %s; rows %v", callStatus(third), third.Meta.ThreadID, forks[0].ID, kinds(e.rows("third")))
	}
	e.expectCompletedOnce(res, "third", third.CallID+"/t1")
	if n := e.count("thread/fork"); n != 1 {
		t.Errorf("%d thread/fork requests: the fork was found, not made again", n)
	}
	if got := kinds(e.rows("third")); !slices.Equal(got, []string{"prepared", "fork_requested", "fork_started", "turn_requested", "turn_started", "completed"}) {
		t.Errorf("rows %v", got)
	}
	started := e.row("third", engine.EvForkStarted).Data
	if started["recovered"] != true || started["thread_id"] != forks[0].ID {
		t.Errorf("fork_started %v", started)
	}
	if n := e.count("turn/start"); n != 4 {
		t.Errorf("%d turn/start requests, want the prefix's three and the call's one", n)
	}
	lists := e.requests("thread/list")
	if len(lists) == 0 || lists[0].Params["cwd"] != restored {
		t.Errorf("thread/list %v", lists)
	}
}

// A turn/start whose answer was lost started a turn that is still running: reconcile finds it by its clientId marker,
// records turn_started (recovered) and the call waits for that turn - no second turn is sent.
func TestAckLostTurnStartAdoptsRunningTurn(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	gate := make(chan struct{})
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, Gate: gate}
	})
	e.lostAck("turn/start", 1)
	first := e.run(buildScript)
	a := calls(first)["a"]
	if a.Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s", callStatus(a))
	}
	if last := kinds(e.rows("a")); last[len(last)-1] != engine.EvTurnRequested {
		t.Fatalf("rows %v", last)
	}
	e.restart("proc-2")
	// the turn ends once the new process has attached to its thread (thread/resume), not before
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if e.host.WaitCount(ctx, "thread/resume", 1) == nil {
			close(gate)
		}
	}()
	res := e.resumeRun(buildScript)
	e.expectCompletedOnce(res, "a", a.CallID+"/t1")
	started := e.row("a", engine.EvTurnStarted).Data
	if started["recovered"] != true || started["op"] != a.CallID+"/t1" || started["turn_id"] == nil {
		t.Errorf("turn_started %v", started)
	}
	if n := e.count("turn/start"); n != 1 {
		t.Errorf("%d turn/start requests", n)
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests", n)
	}
}

// The repair turn of a call whose first answer failed the schema: its turn/start answer was lost, the host ran it. The
// recovered call reads that turn, validates its answer and completes, with exactly one t1 and one t2 on the host.
func TestAckLostRepairTurnNoDuplicate(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if r.N == 1 {
			return fakehost.TurnPlan{Message: "not json"}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	e.lostAck("turn/start", 2)
	script := `const S = ` + verdictSchema + `
const r = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'a' })
return [r.status, r.reason, r.meta.repairUsed]`
	first := e.run(script)
	a := calls(first)["a"]
	if a.Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s", callStatus(a))
	}
	if last := e.rows("a"); last[len(last)-1].Event != engine.EvTurnRequested || last[len(last)-1].Data["k"] != 2 {
		t.Fatalf("rows %v", kinds(e.rows("a")))
	}
	e.restart("proc-2")
	res := e.resumeRun(script)
	e.expectCompletedOnce(res, "a", a.CallID+"/t1", a.CallID+"/t2")
	if got := value[[]any](t, res); fmt.Sprint(got) != "[completed <nil> true]" {
		t.Errorf("%v", got)
	}
	if n := e.count("turn/start"); n != 2 {
		t.Errorf("%d turn/start requests, want the first turn and the repair", n)
	}
	if recovered := e.rowsOf("a", engine.EvTurnStarted); len(recovered) != 2 || recovered[1].Data["recovered"] != true || recovered[1].Data["k"] != 2 {
		t.Errorf("turn_started rows %v", recovered)
	}
}

// The fix turn: a continuation call on the thread of a completed build; its turn/start answer was lost. The completed build
// is not run again, and the fix's turn is adopted by its marker.
func TestAckLostFixTurnNoDuplicate(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.lostAck("turn/start", 2)
	script := `const a = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'a' })
const fix = await agent.call('fix it', { resume: a, label: 'fix' })
return [a.status, fix.status]`
	first := e.run(script)
	by := calls(first)
	if by["a"].Status != "completed" || by["fix"].Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s %s", callStatus(by["a"]), callStatus(by["fix"]))
	}
	e.restart("proc-2")
	res := e.resumeRun(script)
	e.expectCompletedOnce(res, "fix", by["a"].CallID+"/t1", by["fix"].CallID+"/t1")
	if got := value[[]string](t, res); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Errorf("%v", got)
	}
	if n := e.count("turn/start"); n != 2 {
		t.Errorf("%d turn/start requests, want the build's and the fix's", n)
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests: the build is replayed, not run again", n)
	}
	fix := calls(res)["fix"]
	if fix.Meta.ThreadID != by["a"].Meta.ThreadID {
		t.Errorf("the fix runs on %s, the build's thread is %s", fix.Meta.ThreadID, by["a"].Meta.ThreadID)
	}
}

// seedExecutor places a turnless thread of the executor's pair at cwd.
func seedExecutor(e *env, cwd string, created time.Time) string {
	return e.threads.Seed(fakehost.SeedThread{Cwd: cwd, Model: executorModel, Effort: executorEffort, CreatedAt: created})
}

// Two empty threads at the call's cwd (the first request's and another) are interchangeable containers: the one with the
// earliest createdAt is adopted and the other is archived, with an orphan row.
func TestTwoTurnlessCandidatesAdoptEarliestArchiveRest(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	clk := e.useClock()
	e.lostAck("thread/start", 1)
	a := calls(e.run(buildScript))["a"]
	genuine := e.threads.All()[0].ID
	earlier := seedExecutor(e, e.wt("n1", 1, 0), clk.Now().Add(-time.Second))
	e.restart("proc-2")
	res := e.resumeRun(buildScript)
	e.expectCompletedOnce(res, "a", a.CallID+"/t1")
	if got := calls(res)["a"].Meta.ThreadID; got != earlier {
		t.Errorf("the call runs on %s, want the earlier thread %s", got, earlier)
	}
	if got := e.threads.Archived(); !slices.Equal(got, []string{genuine}) {
		t.Errorf("archived %v, want [%s]", got, genuine)
	}
	orphans := e.flows("orphan_thread")
	if len(orphans) != 1 || orphans[0].Data["thread_id"] != genuine || orphans[0].Data["archived"] != true || orphans[0].CallID != a.CallID {
		t.Errorf("orphan rows %v", orphans)
	}
	for _, r := range e.requests("turn/start") {
		if r.Params["threadId"] != earlier {
			t.Errorf("a turn went to %v", r.Params["threadId"])
		}
	}
	if started := e.row("a", engine.EvThreadStarted).Data; started["thread_id"] != earlier {
		t.Errorf("thread_started %v", started)
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests", n)
	}
}

// A thread at the call's cwd that holds a turn is not an empty container of this call - no turn of the call was sent before
// thread_started was recorded - so the outcome is unknown and the node is held. Nothing is sent.
func TestCandidateWithTurnsIsForeignHold(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	clk := e.useClock()
	e.lostAck("thread/start", 1)
	a := calls(e.run(buildScript))["a"]
	e.threads.Seed(fakehost.SeedThread{Cwd: e.wt("n1", 1, 0), Model: executorModel, Effort: executorEffort, CreatedAt: clk.Now(),
		Turns: []fakehost.SeedTurn{{ClientID: "someone-else/t1", Text: "crw-flow-op: someone-else/t1\n\nwork"}}})
	e.restart("proc-2")
	res := e.resumeRun(buildCall + "const after = await agent.call('retry it', { agentType: 'executor', node: 'n1', label: 'b' })\nreturn [r.status, r.reason, after.status, after.reason]")
	if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown", "held", "recovery_hold"}) {
		t.Fatalf("%v", got)
	}
	unknown := e.expectUnknown("a", "n1")
	if !strings.Contains(fmt.Sprint(unknown.Data["detail"]), "holds 1 turn") {
		t.Errorf("detail %v", unknown.Data["detail"])
	}
	if n := e.count("turn/start"); n != 0 {
		t.Errorf("%d turn/start requests: nothing may be sent for an unknown call", n)
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests, want only the lost one (and none for the retry on the held node)", n)
	}
	if len(e.threads.Archived()) != 0 {
		t.Errorf("archived %v: a foreign thread is never archived", e.threads.Archived())
	}
	_ = a
}

// While the connection that carried the thread/start is alive and its answer is pending, an empty listing is not an
// absence: the engine waits and never sends again.
func TestEmptyListingWhileConnectionOpenWaitsNoResend(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	clk := e.useClock()
	e.client.BoundAckOn("thread/start", 150*time.Millisecond)
	// the host has not made the thread and has not answered: the connection stays open
	e.host.Script("thread/start", fakehost.Reply{Delay: 60 * time.Second})
	first := e.run(buildScript)
	if a := calls(first)["a"]; a.Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s", callStatus(a))
	}
	e.client.BoundAckOn("thread/start", 0)
	res := e.resumeRun(buildCall + "const b = await agent.call('retry it', { agentType: 'executor', node: 'n1', label: 'b' })\nreturn [r.status, b.status, b.reason]")
	if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "held", "recovery_hold"}) {
		t.Fatalf("%v", got)
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests: a creation whose answer is pending is never sent again", n)
	}
	if n := e.count("turn/start"); n != 0 {
		t.Errorf("%d turn/start requests", n)
	}
	if rows := e.rowsOf("a", engine.EvThreadRequested); len(rows) != 1 {
		t.Errorf("%d thread_requested rows", len(rows))
	}
	if hasEvent(e.rows("a"), engine.EvOutcomeUnknown) {
		t.Errorf("rows %v: the outcome is not decided while the answer may still come", kinds(e.rows("a")))
	}
	if e.count("thread/list") < 2 {
		t.Errorf("%d listings: the engine looks again while it waits", e.count("thread/list"))
	}
	if clk.waited() <= 0 {
		t.Errorf("the engine did not wait")
	}
}

// With the connection closed and two minutes passed, a second complete listing that is still empty is the proof that the
// first request made nothing: the thread/start is sent again with the next send ordinal. A thread of the first request that
// shows up afterwards still holds no turn and is archived as an orphan.
func TestEmptyListingAfterCloseAndGraceResends(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	clk := e.useClock()
	e.host.Script("thread/start", fakehost.Reply{Close: &fakehost.CloseFrame{Code: websocket.StatusInternalError, Reason: "gone"}})
	a := calls(e.run(buildScript))["a"]
	if a.Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s", callStatus(a))
	}
	res := e.resumeRun(buildScript)
	e.expectCompletedOnce(res, "a", a.CallID+"/t1")
	if got := clk.waited(); got < 2*time.Minute {
		t.Errorf("waited %v, the creation grace is 2 minutes", got)
	}
	seen := e.flows("creation_closed_seen")
	if len(seen) != 1 || seen[0].Data["send"] != 1 {
		t.Errorf("creation_closed_seen rows %v", seen)
	}
	if n := e.count("thread/list"); n != 2 {
		t.Errorf("%d listings, want the one before the grace and the second after it", n)
	}
	reqs := e.rowsOf("a", engine.EvThreadRequested)
	if len(reqs) != 2 || reqs[1].Data["send"] != 2 || reqs[1].Data["cwd"] != reqs[0].Data["cwd"] {
		t.Fatalf("thread_requested rows %v", reqs)
	}
	if n := e.count("thread/start"); n != 2 {
		t.Errorf("%d thread/start requests, want the lost one and the one sent again", n)
	}
	ready := e.flows("creation_resendable")
	if len(ready) != 1 || ready[0].Data["proof"] != "connection_closed" {
		t.Errorf("resend proof rows %v", ready)
	}
	// the first request's thread appears late: still without a turn, it is an orphan
	late := seedExecutor(e, e.wt("n1", 1, 0), clk.Now())
	if _, err := engine.Reconcile(context.Background(), e.journal, e.reconcileConfig(clk)); err != nil {
		t.Fatal(err)
	}
	if got := e.threads.Archived(); !slices.Equal(got, []string{late}) {
		t.Errorf("archived %v, want the late thread [%s]", got, late)
	}
	orphans := e.flows("orphan_thread")
	if len(orphans) != 1 || orphans[0].Data["thread_id"] != late || orphans[0].Data["reason"] != "late_first_request" || orphans[0].Data["archived"] != true {
		t.Errorf("orphan rows %v", orphans)
	}
	if n := e.count("turn/start"); n != 1 {
		t.Errorf("%d turn/start requests", n)
	}
	for _, th := range e.threads.All() {
		if th.ID == late && len(th.Turns) != 0 {
			t.Errorf("the late thread holds %d turns", len(th.Turns))
		}
	}
}

// The process is killed after an intent row was written and before the request was sent: the host holds nothing of the
// call, the dead process's connection is gone, and after the grace a second empty listing lets the request be sent - once.
func TestKillAfterIntentBeforeSend(t *testing.T) {
	t.Run("thread/start", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		clk := e.useClock()
		e.killAfter(engine.EvThreadRequested)
		a := calls(e.run(buildScript))["a"]
		if n := e.count("thread/start"); n != 0 {
			t.Fatalf("%d thread/start requests reached the host before the kill", n)
		}
		e.thaw()
		e.restart("proc-2")
		res := e.resumeRun(buildScript)
		e.expectCompletedOnce(res, "a", a.CallID+"/t1")
		if n := e.count("thread/start"); n != 1 {
			t.Errorf("%d thread/start requests, want the one sent after the proof", n)
		}
		if clk.waited() < 2*time.Minute {
			t.Errorf("waited %v before sending again", clk.waited())
		}
		if reqs := e.rowsOf("a", engine.EvThreadRequested); len(reqs) != 2 || reqs[1].Data["send"] != 2 {
			t.Errorf("thread_requested rows %v", reqs)
		}
	})
	t.Run("turn/start", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.killAfter(engine.EvTurnRequested)
		a := calls(e.run(buildScript))["a"]
		if n := e.count("turn/start"); n != 0 {
			t.Fatalf("%d turn/start requests reached the host before the kill", n)
		}
		e.thaw()
		e.restart("proc-2")
		res := e.resumeRun(buildScript)
		e.expectCompletedOnce(res, "a", a.CallID+"/t1")
		if n := e.count("turn/start"); n != 1 {
			t.Errorf("%d turn/start requests", n)
		}
		if n := e.count("thread/start"); n != 1 {
			t.Errorf("%d thread/start requests", n)
		}
		if reqs := e.rowsOf("a", engine.EvTurnRequested); len(reqs) != 2 || reqs[1].Data["resend"] != true {
			t.Errorf("turn_requested rows %v", reqs)
		}
		if len(e.flows("creation_resendable")) != 1 {
			t.Errorf("the absence proof of the turn was not recorded")
		}
	})
}

// The process is killed after the request was sent and before its outcome was recorded: the thread or the turn exists on
// the host, and reconcile adopts it - nothing is sent again.
func TestKillAfterSendBeforeRecord(t *testing.T) {
	t.Run("thread/start", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.killBefore(engine.EvThreadStarted)
		a := calls(e.run(buildScript))["a"]
		if n := e.count("thread/start"); n != 1 {
			t.Fatalf("%d thread/start requests in the first process", n)
		}
		if hasEvent(e.rows("a"), engine.EvThreadStarted) {
			t.Fatalf("rows %v: the kill must come before thread_started", kinds(e.rows("a")))
		}
		e.thaw()
		e.restart("proc-2")
		res := e.resumeRun(buildScript)
		e.expectCompletedOnce(res, "a", a.CallID+"/t1")
		if n := e.count("thread/start"); n != 1 {
			t.Errorf("%d thread/start requests: the thread was found", n)
		}
		if started := e.row("a", engine.EvThreadStarted).Data; started["recovered"] != true {
			t.Errorf("thread_started %v", started)
		}
	})
	t.Run("turn/start", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.killBefore(engine.EvTurnStarted)
		a := calls(e.run(buildScript))["a"]
		if n := e.count("turn/start"); n != 1 {
			t.Fatalf("%d turn/start requests in the first process", n)
		}
		if hasEvent(e.rows("a"), engine.EvTurnStarted) {
			t.Fatalf("rows %v: the kill must come before turn_started", kinds(e.rows("a")))
		}
		e.thaw()
		e.restart("proc-2")
		res := e.resumeRun(buildScript)
		e.expectCompletedOnce(res, "a", a.CallID+"/t1")
		if n := e.count("turn/start"); n != 1 {
			t.Errorf("%d turn/start requests: the turn was found", n)
		}
		if started := e.row("a", engine.EvTurnStarted).Data; started["recovered"] != true {
			t.Errorf("turn_started %v", started)
		}
	})
}

func turnText(params map[string]any) string {
	input, _ := params["input"].([]any)
	if len(input) == 0 {
		return ""
	}
	m, _ := input[0].(map[string]any)
	s, _ := m["text"].(string)
	return s
}

// Model failures, policy refusals and an unknown outcome on one path and on parallel paths each keep their own status and
// reason, and the call whose outcome is unknown has exactly one turn on the host and is never sent again.
func TestMixedFailuresSamePathAndParallel(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	// One call runs at a time. The parallel paths stay three paths with three positions, but the request whose answer is lost
	// closes the connection every call shares, and a call that had a request in flight on it would then be unknown too, which
	// is true of the host and would make the statuses depend on timing.
	e.pool = 1
	badRequest := fakehost.ErrorWithInfo(-32600, "the request is malformed", "badRequest")
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		switch {
		case strings.Contains(r.Text, "null-model"):
			return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "too long", CodexErrorInfo: "contextWindowExceeded"}}
		case strings.Contains(r.Text, "refuse-me"):
			return fakehost.TurnPlan{StartError: badRequest}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	e.threads.DropAck("turn/start", func(_ int, params map[string]any) bool { return strings.Contains(turnText(params), "lose-me") })
	res := e.run(`const seqA = await agent.call('null-model first', { agentType: 'executor', node: 'nA', label: 'seq-null' })
const seqB = await agent.call('refuse-me second', { agentType: 'executor', node: 'nA', label: 'seq-refused' })
const par = await parallel([
  async () => agent.call('null-model parallel', { agentType: 'executor', node: 'nB', label: 'par-null' }),
  async () => agent.call('refuse-me parallel', { agentType: 'executor', node: 'nC', label: 'par-refused' }),
  async () => agent.call('lose-me parallel', { agentType: 'executor', node: 'nD', label: 'par-unknown' }),
])
const again = await agent.call('lose-me again', { agentType: 'executor', node: 'nD', label: 'again' })
return [seqA, seqB, ...par, again].map((r) => r.status + '/' + r.reason)`)
	got := value[[]string](t, res)
	want := []string{
		"null_model/context_window_exceeded", "refused/host_config_badRequest",
		"null_model/context_window_exceeded", "refused/host_config_badRequest", "unknown/dispatch_unknown", "held/recovery_hold",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("%v, want %v", got, want)
	}
	by := calls(res)
	if n := e.markerCount(by["par-unknown"].CallID + "/t1"); n != 1 {
		t.Errorf("the unknown call has %d turn/start requests, want 1", n)
	}
	for _, label := range []string{"seq-null", "seq-refused", "par-null", "par-refused", "par-unknown"} {
		if n := e.markerCount(by[label].CallID + "/t1"); n != 1 {
			t.Errorf("%s: %d turn/start requests for its operation", label, n)
		}
	}
	if n := e.count("thread/start"); n != 5 {
		t.Errorf("%d thread/start requests: the call on the held node starts no thread", n)
	}
	if hasEvent(e.rows("par-unknown"), engine.EvOutcomeUnknown) || hasEvent(e.rows("par-unknown"), engine.EvHeld) {
		t.Errorf("rows %v: a lost request leaves its intent as the last row", kinds(e.rows("par-unknown")))
	}
	if n := e.markerCount(by["again"].CallID + "/t1"); n != 0 {
		t.Errorf("a call on the node of the unknown call sent %d turns", n)
	}
}

// A host that cannot be read - the listing fails, the history of a candidate fails, the history of the thread fails - never
// shows an absence: the outcome is unknown, the node is held and nothing is sent.
func TestUnreadableHostHoldsUnknown(t *testing.T) {
	fail := fakehost.Reply{ErrorObject: map[string]any{"code": -32603, "message": "the host cannot answer"}}
	t.Run("thread listing", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.lostAck("thread/start", 1)
		e.run(buildScript)
		e.restart("proc-2")
		e.host.Script("thread/list", fail)
		res := e.resumeRun(buildCall + "return [r.status, r.reason]")
		if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
			t.Fatalf("%v", got)
		}
		unknown := e.expectUnknown("a", "n1")
		if !strings.Contains(fmt.Sprint(unknown.Data["detail"]), "listing") {
			t.Errorf("detail %v", unknown.Data["detail"])
		}
		if e.count("thread/start") != 1 || e.count("turn/start") != 0 {
			t.Errorf("%d thread/start, %d turn/start requests", e.count("thread/start"), e.count("turn/start"))
		}
	})
	t.Run("candidate history", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.lostAck("thread/start", 1)
		e.run(buildScript)
		e.restart("proc-2")
		e.host.Script("thread/turns/list", fail)
		res := e.resumeRun(buildCall + "return [r.status, r.reason]")
		if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
			t.Fatalf("%v", got)
		}
		e.expectUnknown("a", "n1")
		if e.count("thread/start") != 1 || e.count("turn/start") != 0 {
			t.Errorf("%d thread/start, %d turn/start requests", e.count("thread/start"), e.count("turn/start"))
		}
	})
	t.Run("turn history", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.lostAck("turn/start", 1)
		a := calls(e.run(buildScript))["a"]
		e.restart("proc-2")
		e.host.Script("thread/turns/list", fail)
		res := e.resumeRun(buildCall + "return [r.status, r.reason]")
		if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
			t.Fatalf("%v", got)
		}
		e.expectUnknown("a", "n1")
		if n := e.markerCount(a.CallID + "/t1"); n != 1 {
			t.Errorf("%d turn/start requests carried the operation: nothing is sent when the history cannot be read", n)
		}
	})
	t.Run("hold survives a resume", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.lostAck("thread/start", 1)
		e.run(buildScript)
		e.restart("proc-2")
		e.host.Script("thread/list", fail)
		e.resumeRun(buildCall + "return r.status")
		// the host can be read again: the same reconcile now establishes the outcome and the hold ends
		res := e.resumeRun(buildScript)
		if c := calls(res)["a"]; c.Status != engine.StatusCompleted {
			t.Fatalf("a: %s; rows %v", callStatus(c), kinds(e.rows("a")))
		}
		if n := e.count("turn/start"); n != 1 || e.count("thread/start") != 1 {
			t.Errorf("%d turn/start, %d thread/start requests", n, e.count("thread/start"))
		}
		if got := kinds(e.rows("a")); !slices.Equal(got, []string{"prepared", "thread_requested", "outcome_unknown", "thread_started", "turn_requested", "turn_started", "completed"}) {
			t.Errorf("rows %v", got)
		}
		if len(engine.RecoveryHolds(e.journal, e.runID)) != 0 {
			t.Errorf("the hold did not end: %v", engine.RecoveryHolds(e.journal, e.runID))
		}
	})
}

// A turn whose items the list returns as a summary and thread/items/list cannot give is neither absent nor present: no
// turn is sent again, the outcome is unknown. When thread/items/list can give the items the turn is read and adopted.
func TestReconcileSummaryTurnNoResend(t *testing.T) {
	itemsFail := map[string]any{"code": -32600, "message": "items are not available"}
	t.Run("unreadable", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
			return fakehost.TurnPlan{Message: passMessage, SummaryOnly: true, ItemsListError: itemsFail}
		})
		e.lostAck("turn/start", 1)
		a := calls(e.run(buildScript))["a"]
		e.restart("proc-2")
		res := e.resumeRun(buildScript + "\n")
		if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
			t.Fatalf("%v", got)
		}
		e.expectUnknown("a", "n1")
		if n := e.markerCount(a.CallID + "/t1"); n != 1 {
			t.Errorf("%d turn/start requests carried the operation, want the one that was lost and none after", n)
		}
		if e.count("thread/items/list") == 0 {
			t.Errorf("the items of the summary turn were not asked for")
		}
		if n := e.count("turn/start"); n != 1 {
			t.Errorf("%d turn/start requests", n)
		}
	})
	t.Run("readable through thread/items/list", func(t *testing.T) {
		e := newEnv(t, withOwner("proc-1"))
		e.useClock()
		e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
			return fakehost.TurnPlan{Message: passMessage, SummaryOnly: true}
		})
		e.lostAck("turn/start", 1)
		a := calls(e.run(buildScript))["a"]
		e.restart("proc-2")
		res := e.resumeRun(buildScript)
		e.expectCompletedOnce(res, "a", a.CallID+"/t1")
		if e.count("thread/items/list") == 0 {
			t.Errorf("the summary turn was not read with thread/items/list")
		}
	})
}

// A turn that is notLoaded and cannot be read through thread/items/list is an unreadable state, never an absence: the call
// ends unknown and the node is in recovery_hold.
func TestReconcileNotLoadedTurnHoldsUnknown(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan {
		return fakehost.TurnPlan{Message: passMessage, NotLoaded: true, ItemsListError: map[string]any{"code": -32600, "message": "not loaded"}}
	})
	e.lostAck("turn/start", 1)
	a := calls(e.run(buildScript))["a"]
	e.restart("proc-2")
	res := e.resumeRun(buildCall + "const b = await agent.call('retry it', { agentType: 'executor', node: 'n1', label: 'b' })\nreturn [r.status, b.status, b.reason]")
	if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "held", "recovery_hold"}) {
		t.Fatalf("%v", got)
	}
	e.expectUnknown("a", "n1")
	if n := e.markerCount(a.CallID + "/t1"); n != 1 || e.count("turn/start") != 1 {
		t.Errorf("%d turn/start requests carried the operation, %d in all", n, e.count("turn/start"))
	}
	if n := e.count("thread/start"); n != 1 {
		t.Errorf("%d thread/start requests: the held node starts no new attempt", n)
	}
}

// A null_model rebuild starts its own thread in its own attempt worktree; when that thread/start's answer is lost, the
// only thread at that cwd is the rebuild's and it is adopted - the first build's thread (with its turn) is at another cwd.
func TestNullModelRebuildAckLostAdoptsByOwnCwd(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	e.useClock()
	e.plan(func(r fakehost.TurnRequest) fakehost.TurnPlan {
		if strings.Contains(r.Text, "first build") {
			return fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "too long", CodexErrorInfo: "contextWindowExceeded"}}
		}
		return fakehost.TurnPlan{Message: passMessage}
	})
	e.lostAck("thread/start", 2)
	script := `const a = await agent.call('first build', { agentType: 'executor', node: 'n1', label: 'a' })
const b = await agent.call('rebuild', { agentType: 'executor', node: 'n1', label: 'b' })
return [a.status, b.status, b.reason]`
	first := calls(e.run(script))
	if first["a"].Status != engine.StatusNullModel || first["b"].Status != engine.StatusUnknown {
		t.Fatalf("the first run: %s %s", callStatus(first["a"]), callStatus(first["b"]))
	}
	rebuildCwd := e.wt("n1", 2, 0)
	if cwd := e.row("b", engine.EvThreadRequested).Data["cwd"]; cwd != rebuildCwd {
		t.Fatalf("the rebuild's cwd is %v, want %s", cwd, rebuildCwd)
	}
	e.restart("proc-2")
	res := e.resumeRun(script)
	if got := value[[]any](t, res); fmt.Sprint(got) != "[null_model completed <nil>]" {
		t.Fatalf("%v", got)
	}
	e.expectCompletedOnce(res, "b", first["b"].CallID+"/t1")
	if hasEvent(e.rows("a"), engine.EvOutcomeUnknown) {
		t.Errorf("the first build's rows %v", kinds(e.rows("a")))
	}
	if n := e.count("thread/start"); n != 2 {
		t.Errorf("%d thread/start requests, want the two builds'", n)
	}
	if n := e.count("turn/start"); n != 2 {
		t.Errorf("%d turn/start requests, want one per build", n)
	}
	lists := e.requests("thread/list")
	if len(lists) == 0 || lists[0].Params["cwd"] != rebuildCwd {
		t.Errorf("thread/list %v: the rebuild is looked up by its own cwd", lists)
	}
	if len(e.threads.Archived()) != 0 {
		t.Errorf("archived %v", e.threads.Archived())
	}
}

// A call whose last event is held is terminal: the first step of a resume does not look it up on the host. (What the resume
// then does with the held position is F07's rule (g); this build runs it as a call of its own.)
func TestHeldCallNotReconciled(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	clk := e.useClock()
	e.plan(failing("serverOverloaded"))
	first := calls(e.run(buildScript))["a"]
	if first.Status != engine.StatusHeld || first.Reason != "host_infra_serverOverloaded" {
		t.Fatalf("the first run: %s", callStatus(first))
	}
	e.restart("proc-2")
	before := len(e.host.Requests())
	rep, err := engine.Reconcile(context.Background(), e.journal, e.reconcileConfig(clk))
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.ByCall) != 0 {
		t.Errorf("reconcile decided %v for a held call", rep.ByCall)
	}
	if after := len(e.host.Requests()); after != before {
		t.Errorf("reconcile sent %d requests for a call whose last event is held", after-before)
	}
	// a whole resume: the first step reads nothing of the host's threads or turns
	e.plan(func(fakehost.TurnRequest) fakehost.TurnPlan { return fakehost.TurnPlan{Message: passMessage} })
	e.resumeRun(buildScript)
	if n := e.count("thread/list") + e.count("thread/turns/list"); n != 0 {
		t.Errorf("%d thread/list and thread/turns/list requests during the resume", n)
	}
}

// A fork whose answer was lost, together with a second exact-prefix fork at the same cwd: they are interchangeable, the one
// with the earlier createdAt is adopted and the other is archived with an orphan row.
func TestForkAckLostTwoExactPrefixAdoptEarliest(t *testing.T) {
	e := newEnv(t, withOwner("proc-1"))
	clk := e.useClock()
	restored, script := e.restoreFork()
	e.lostAck("thread/fork", 1)
	by := calls(e.run(script))
	source := by["first"].Meta.ThreadID
	genuine := e.forkThreads(source)
	if len(genuine) != 1 {
		t.Fatalf("the host holds %d forks", len(genuine))
	}
	mid := by["mid"].Meta.TurnIDs
	earlier := e.threads.Seed(fakehost.SeedThread{Cwd: restored, Model: executorModel, Effort: executorEffort, CreatedAt: clk.Now().Add(-time.Second),
		ForkedFromID: source, CopyTurnsOf: source, ThroughTurn: mid[len(mid)-1]})
	e.restart("proc-2")
	res := e.resumeRun(script)
	third := calls(res)["third"]
	if third.Status != engine.StatusCompleted || third.Meta.ThreadID != earlier {
		t.Fatalf("third: %s on %s, want completed on the earlier fork %s; rows %v", callStatus(third), third.Meta.ThreadID, earlier, kinds(e.rows("third")))
	}
	e.expectCompletedOnce(res, "third", third.CallID+"/t1")
	if got := e.threads.Archived(); !slices.Equal(got, []string{genuine[0].ID}) {
		t.Errorf("archived %v, want [%s]", got, genuine[0].ID)
	}
	orphans := e.flows("orphan_thread")
	if len(orphans) != 1 || orphans[0].Data["thread_id"] != genuine[0].ID || orphans[0].Data["archived"] != true {
		t.Errorf("orphan rows %v", orphans)
	}
	if n := e.count("thread/fork"); n != 1 {
		t.Errorf("%d thread/fork requests", n)
	}
	if started := e.row("third", engine.EvForkStarted).Data; started["thread_id"] != earlier || started["recovered"] != true {
		t.Errorf("fork_started %v", started)
	}
}

// A candidate fork that is not exactly the recorded prefix is not this call's: one with a turn after through_turn_id, one
// whose prefix has its turns in another order, and one whose prefix turn cannot be read in full are each an unknown outcome,
// and thread/fork is never sent again.
func TestForkAckLostBoundaryMismatchIsForeign(t *testing.T) {
	cases := map[string]struct {
		damage func(e *env, fork string)
		turns  int
	}{
		"a turn after through_turn_id": {turns: 4, damage: func(e *env, fork string) {
			e.restart("proc-1b")
			if _, err := e.client.Call(context.Background(), "turn/start", map[string]any{"threadId": fork, "input": []any{map[string]any{"type": "text", "text": "after the boundary"}}}); err != nil {
				t.Fatal(err)
			}
		}},
		"the prefix in another order": {turns: 3, damage: func(e *env, fork string) { e.threads.SwapTurns(fork, 0, 1) }},
		"a prefix turn not loaded": {turns: 3, damage: func(e *env, fork string) {
			e.threads.ReadAs(fork, 1, fakehost.TurnPlan{NotLoaded: true, ItemsListError: map[string]any{"code": -32600, "message": "not loaded"}})
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newEnv(t, withOwner("proc-1"))
			e.useClock()
			_, script := e.restoreFork()
			e.lostAck("thread/fork", 1)
			by := calls(e.run(script))
			forks := e.forkThreads(by["first"].Meta.ThreadID)
			if len(forks) != 1 {
				t.Fatalf("the host holds %d forks", len(forks))
			}
			c.damage(e, forks[0].ID)
			e.restart("proc-2")
			res := e.resumeRun(script)
			if third := calls(res)["third"]; third.Status != engine.StatusUnknown || third.Reason != engine.ReasonDispatchUnknown {
				t.Fatalf("third: %s; rows %v", callStatus(third), kinds(e.rows("third")))
			}
			unknown := e.expectUnknown("third", "n1")
			t.Logf("detail: %v", unknown.Data["detail"])
			if n := e.count("thread/fork"); n != 1 {
				t.Errorf("%d thread/fork requests: nothing is sent again for an unknown fork", n)
			}
			if n := e.count("turn/start"); n != c.turns {
				t.Errorf("%d turn/start requests, want %d (the prefix's, and none for the call)", n, c.turns)
			}
			if len(e.threads.Archived()) != 0 {
				t.Errorf("archived %v: a candidate that is not the prefix is not archived", e.threads.Archived())
			}
		})
	}
}

// A new attempt starts only after the turn of the attempt it replaces has ended: a turn the previous process left running
// is interrupted, the interruption is confirmed with thread/read, the old call ends superseded, and only then does the new
// thread start.
func TestSupersedeInterruptsLiveTurnFirst(t *testing.T) {
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
		dead <- e.runWith(ctx, `const a = await agent.call('first attempt', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	}()
	select {
	case <-started:
	case <-time.After(30 * time.Second):
		t.Fatal("the first attempt's turn did not start")
	}
	// the process dies with its turn still running on the host
	e.journal.freeze()
	e.thaw()
	e.restart("proc-2")
	firstThread := e.threads.All()[0].ID
	if st := e.threads.All()[0].Turns[0].Status; st != "inProgress" {
		t.Fatalf("the first attempt's turn is %s", st)
	}
	res := e.resumeRun(`const a = await agent.call('second attempt', { agentType: 'executor', node: 'n1', label: 'a' }); return a.status`)
	if c := calls(res)["a"]; c.Status != engine.StatusCompleted {
		t.Fatalf("the new attempt: %s; rows %v", callStatus(c), kinds(e.rows("a")))
	}
	if got := e.threads.Interrupts(); !slices.Equal(got, []string{firstThread}) {
		t.Errorf("interrupted threads %v, want [%s]", got, firstThread)
	}
	if st := e.threads.All()[0].Turns[0].Status; st != "interrupted" {
		t.Errorf("the old attempt's turn is %s after the interrupt", st)
	}
	// the order on the host: interrupt, then the confirming thread/read, then the new thread
	var interrupt, confirm, secondStart = -1, -1, -1
	starts := 0
	for i, r := range e.host.Requests() {
		switch r.Method {
		case "turn/interrupt":
			interrupt = i
		case "thread/read":
			if interrupt >= 0 && confirm < 0 {
				confirm = i
			}
		case "thread/start":
			if starts++; starts == 2 {
				secondStart = i
			}
		}
	}
	if interrupt < 0 || confirm < interrupt || secondStart < confirm {
		t.Errorf("request order: interrupt %d, confirming thread/read %d, new thread/start %d", interrupt, confirm, secondStart)
	}
	var superseded, secondRequest = -1, -1
	requests := 0
	for i, r := range e.rows("a") {
		switch r.Event {
		case engine.EvSuperseded:
			superseded = i
		case engine.EvThreadRequested:
			if requests++; requests == 2 {
				secondRequest = i
			}
		}
	}
	if superseded < 0 || secondRequest < superseded {
		t.Errorf("rows %v: superseded must come before the new attempt's thread_requested", kinds(e.rows("a")))
	}
	if row := e.rowsOf("a", engine.EvSuperseded); len(row) != 1 || row[0].Data["reason"] != engine.ReasonSupersededByAttempt {
		t.Errorf("superseded rows %v", row)
	}
	// the dead process's run ends once its connection is gone
	cancel()
	select {
	case <-dead:
	case <-time.After(30 * time.Second):
		t.Error("the first process's run did not end")
	}
}

// holdOnAForeignThread leaves a call unknown: its thread/start answer was lost and a thread with a turn sits at its cwd.
func holdOnAForeignThread(t *testing.T) (e *env, clk *fakeClock, genuine, foreign string) {
	t.Helper()
	e = newEnv(t, withOwner("proc-1"))
	clk = e.useClock()
	e.lostAck("thread/start", 1)
	e.run(buildScript)
	genuine = e.threads.All()[0].ID
	foreign = e.threads.Seed(fakehost.SeedThread{Cwd: e.wt("n1", 1, 0), Model: executorModel, Effort: executorEffort, CreatedAt: clk.Now(),
		Turns: []fakehost.SeedTurn{{ClientID: "someone-else/t1", Text: "crw-flow-op: someone-else/t1\n\nwork"}}})
	e.restart("proc-2")
	res := e.resumeRun(buildScript)
	if got := value[[]string](t, res); !slices.Equal(got, []string{"unknown", "dispatch_unknown"}) {
		t.Fatalf("%v", got)
	}
	e.expectUnknown("a", "n1")
	return e, clk, genuine, foreign
}

// The hold of an unknown outcome ends only through reconcile finding the proof, or through `crw flow recover`: --absent is
// refused while the foreign thread is there and accepted, with its evidence, once the host shows only an empty container;
// --adopt names the thread by hand.
func TestRecoverReleasesTheHoldThroughTheBackend(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		e, clk, genuine, foreign := holdOnAForeignThread(t)
		cfg := e.reconcileConfig(clk)
		before := len(e.journal.Events())
		_, err := engine.Recover(context.Background(), e.journal, cfg, engine.RecoverRequest{CallID: e.row("a", engine.EvPrepared).CallID, Absent: true})
		var refused *engine.ProofNotMet
		if !errors.As(err, &refused) || !strings.Contains(err.Error(), "is not this call's") {
			t.Fatalf("%v", err)
		}
		if len(e.journal.Events()) != before {
			t.Fatalf("a refused recovery appended %v", kinds(e.journal.Events()[before:]))
		}
		// the foreign thread goes away
		if _, err := e.client.Call(context.Background(), "thread/archive", map[string]any{"threadId": foreign}); err != nil {
			t.Fatal(err)
		}
		callID := e.row("a", engine.EvPrepared).CallID
		res, err := engine.Recover(context.Background(), e.journal, cfg, engine.RecoverRequest{CallID: callID, Absent: true})
		if err != nil || res.Action != engine.RecoverThreadReady || res.ThreadID != genuine {
			t.Fatalf("%+v / %v", res, err)
		}
		got := kinds(e.journal.Events()[before:])
		if !slices.Equal(got, []string{"recovery_cleared", "thread_started"}) {
			t.Errorf("rows %v", got)
		}
		resumed := e.resumeRun(buildScript)
		if c := calls(resumed)["a"]; c.Status != engine.StatusCompleted || c.Meta.ThreadID != genuine {
			t.Fatalf("a: %s on %s; rows %v", callStatus(c), c.Meta.ThreadID, kinds(e.rows("a")))
		}
		if n := e.markerCount(callID + "/t1"); n != 1 {
			t.Errorf("%d turn/start requests carried the operation", n)
		}
		if len(engine.RecoveryHolds(e.journal, e.runID)) != 0 {
			t.Errorf("holds %v", engine.RecoveryHolds(e.journal, e.runID))
		}
	})
	t.Run("adopt", func(t *testing.T) {
		e, clk, genuine, _ := holdOnAForeignThread(t)
		callID := e.row("a", engine.EvPrepared).CallID
		res, err := engine.Recover(context.Background(), e.journal, e.reconcileConfig(clk), engine.RecoverRequest{CallID: callID, Adopt: genuine})
		if err != nil || res.Action != engine.RecoverThreadReady || res.ThreadID != genuine {
			t.Fatalf("%+v / %v", res, err)
		}
		resumed := e.resumeRun(buildScript)
		if c := calls(resumed)["a"]; c.Status != engine.StatusCompleted || c.Meta.ThreadID != genuine {
			t.Fatalf("a: %s on %s; rows %v", callStatus(c), c.Meta.ThreadID, kinds(e.rows("a")))
		}
		if n := e.markerCount(callID + "/t1"); n != 1 {
			t.Errorf("%d turn/start requests carried the operation", n)
		}
	})
}
