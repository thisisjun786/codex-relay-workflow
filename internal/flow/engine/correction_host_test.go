package engine_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// These tests are the correction round of CRW-1200 and CRW-1269 after their pre-merge evaluation (head 7a793656): each is the
// scenario of one reported defect, written to fail on that head.

// stepClock is a clock of whole seconds that only waiting moves: the engine and the fake host read the same one, so a thread the
// host made is inside the window of the request that made it.
type stepClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *stepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *stepClock) Wait(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
	return ctx.Err()
}

// allocationOf is the allocation a prepared row recorded.
func allocationOf(ev engine.Event) map[string]any {
	m, _ := ev.Data["allocation"].(map[string]any)
	return m
}

// CRW-1200 d1: the second continuation of a thread the previous process started, in the same suffix, forks too: the restored
// worktree the first one adopted is the node's worktree, and no turn goes to the original thread whose cwd was retired.
func TestSecondContinuationInASuffixForksIntoTheRestoredWorktree(t *testing.T) {
	e := newEnv(t)
	e.edits(nil)
	e.run(buildScript + `return [b.status]`)
	old := e.wt("n1", 1, 0)
	buildThread := str(e.lastRow("build", engine.EvThreadStarted).Data, "thread_id")
	buildTurn := str(e.lastRow("build", engine.EvTurnStarted).Data, "turn_id")
	turnsBefore := len(e.requests("turn/start"))
	e.newProcess("proc-2")
	e.edits(nil)
	body := buildScript + `const x = await agent.call('xray one', { resume: b, label: 'x' })
const y = await agent.call('yankee two', { resume: b, label: 'y' })
return [b.status, x.status, y.status]`
	if got := value[[]string](t, e.resume(body)); !slices.Equal(got, []string{"completed", "completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	restored := e.wt("n1", 2, 1)
	for _, r := range e.requests("turn/start")[turnsBefore:] {
		if r.Params["threadId"] == buildThread {
			t.Errorf("a turn was sent to the original thread, whose cwd is the retired worktree: %s", markerOf(turnText(r)))
		}
	}
	forks := e.requests("thread/fork")
	if len(forks) != 2 {
		t.Fatalf("%d thread/fork requests, want one for each continuation", len(forks))
	}
	for _, f := range forks {
		if f.Params["threadId"] != buildThread || f.Params["lastTurnId"] != buildTurn || f.Params["cwd"] != restored {
			t.Errorf("thread/fork %+v, want the build thread at %s in %s", f.Params, buildTurn, restored)
		}
	}
	for _, name := range []string{"xray.txt", "yankee.txt"} {
		if _, err := os.Stat(filepath.Join(restored, name)); err != nil {
			t.Errorf("%s is not in the restored worktree: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(old, name)); err == nil {
			t.Errorf("%s was written into the retired worktree", name)
		}
	}
	if n := len(e.flowRows(engine.EvWorktreeRestored)); n != 1 {
		t.Errorf("%d worktree_restored rows", n)
	}
	// the work of both continuations is proven by the restored worktree: the next resume replays all of it
	e.nothingLive(t, "proc-3", body, []string{"completed", "completed", "completed"})
}

// CRW-1200 d4: a held executor goes on in place only when the worktree is exactly the state the hold recorded. Later work of the
// script on the same worktree that happens to prove the held call by a cumulative proof does not make the held call's files
// the files it left: the call is the first position of a suffix, and its P_held is kept.
func TestHeldExecutorIsNotContinuedInPlaceOverLaterWork(t *testing.T) {
	e := newEnv(t)
	e.edits(func(r fakehost.TurnRequest) *fakehost.TurnPlan {
		if r.N == 1 && strings.Contains(r.Text, "heldme") {
			writeFile(t, filepath.Join(e.threadCwd(r.ThreadID), "heldme.txt"), "edited then held\n")
			return &fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
		}
		return nil
	})
	body := `const h = await agent.call('heldme build the thing', { agentType: 'executor', node: 'n1', label: 'h' })
const f = await agent.call('later more work', { resume: h, label: 'f' })
return [h.status, f.status]`
	if got := value[[]string](t, e.run(body)); !slices.Equal(got, []string{"held", "completed"}) {
		t.Fatalf("%v", got)
	}
	held := e.lastRow("h", engine.EvHeld)
	pHeld := str(held.Data, "p_held")
	if pHeld == "" || pHeld == str(e.lastRow("f", engine.EvCompleted).Data, "pending_tree") {
		t.Fatalf("the scenario needs a held state (%q) other than the later call's tree", pHeld)
	}
	e.newProcess("proc-2")
	e.edits(nil)
	if got := value[[]string](t, e.resume(body)); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	if n := len(e.flowRows(engine.EvHoldCleared)); n != 0 {
		t.Errorf("%d hold_cleared rows: the held call went on over files that are not its own", n)
	}
	if n := countOf(kinds(e.rows("h")), engine.EvPrepared); n != 2 {
		t.Errorf("h has %d prepared rows, want a new attempt in a restored worktree", n)
	}
	if rows := e.flowRows(engine.EvWorktreeRestored); len(rows) != 1 || rows[0].Data["C"] != e.base || rows[0].Data["P"] != nil {
		t.Errorf("worktree_restored %+v, want the base boundary", rows)
	}
	heldRef := engine.SupersededHeldRef(e.runID, 1, "root", "n1", held.CallID)
	if got := git(t, e.repo, "rev-parse", heldRef+"^{tree}"); got != pHeld {
		t.Errorf("the held state %s is not kept on %s (%s)", pHeld, heldRef, got)
	}
}

// CRW-1200 d2 and CRW-1269 d3: the checkpoint packet reaches the first turn of a checkpoint thread whose creation was recovered:
// the thread whose answer was lost and that the next process adopts, and the creation the next process proves absent and sends
// again.
func TestCheckpointPacketReachesARecoveredCheckpointThread(t *testing.T) {
	cont := buildScript + `const r = await agent.call('research two', { resume: b, label: 'research' })
return [b.status, r.status]`
	scenario := func(t *testing.T) *env {
		e := newEnv(t)
		e.edits(nil)
		e.run(buildScript + `const r = await agent.call('research one', { resume: b, label: 'research' })
return [b.status, r.status]`)
		e.newProcess("proc-2")
		// the host forgot the threads: the continuation cannot fork
		e.startHost()
		e.clock = &stepClock{t: time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)}
		e.threads.Clock(e.clock.Now)
		// the new host numbers its threads from the start: an unrelated thread takes the id the build thread had in the journal
		e.threads.Seed(fakehost.SeedThread{Cwd: filepath.Join(e.root, "elsewhere"), CreatedAt: e.clock.Now()})
		e.spawn("proc-2")
		e.edits(nil)
		return e
	}
	firstText := func(t *testing.T, e *env) string {
		t.Helper()
		turns := e.requests("turn/start")
		if len(turns) == 0 {
			t.Fatal("the checkpoint thread got no turn")
		}
		return turnText(turns[0])
	}
	check := func(t *testing.T, e *env) {
		t.Helper()
		text := firstText(t, e)
		for _, want := range []string{"build the thing", "research two", "crw-flow-op: "} {
			if !strings.Contains(text, want) {
				t.Errorf("the first turn of the recovered checkpoint thread lacks %q: %q", want, text)
			}
		}
		if n := len(e.requests("thread/start")); n != 1 && n != 2 {
			t.Errorf("%d thread/start requests", n)
		}
	}
	t.Run("the answer of thread/start was lost and the next process adopts the thread", func(t *testing.T) {
		e := scenario(t)
		e.threads.DropAck("thread/start", func(n int, _ map[string]any) bool { return n == 1 })
		e.resume(cont)
		e.newProcess("proc-3")
		e.edits(nil)
		if got := value[[]string](t, e.resume(cont)); !slices.Equal(got, []string{"completed", "completed"}) {
			t.Fatalf("%v", got)
		}
		if n := len(e.requests("thread/start")); n != 1 {
			t.Errorf("%d thread/start requests: the thread was found, not made again", n)
		}
		check(t, e)
	})
	t.Run("the process died after thread_requested and the next process sends it again", func(t *testing.T) {
		e := scenario(t)
		e.killWhen(func(ev engine.Event) bool {
			return ev.Kind == engine.KindCall && ev.Label == "research" && ev.Event == engine.EvThreadRequested
		})
		e.resume(cont)
		if n := len(e.requests("thread/start")); n != 0 {
			t.Fatalf("%d thread/start requests were sent before the process died", n)
		}
		e.newProcess("proc-3")
		e.edits(nil)
		if got := value[[]string](t, e.resume(cont)); !slices.Equal(got, []string{"completed", "completed"}) {
			t.Fatalf("%v", got)
		}
		check(t, e)
	})
}

// CRW-1269 d2: a fresh call that is not an executor's and runs again in a suffix gets a private directory of its own: the
// directory its earlier attempt used still exists and nothing is made over it.
func TestFreshCallRerunInASuffixGetsAnotherPrivateDirectory(t *testing.T) {
	e := newEnv(t)
	e.edits(nil)
	first := `const a = await agent.call('review one', { agentType: 'explorer', label: 'a' })
const b = await agent.call('review two', { agentType: 'explorer', label: 'b' })
return [a.status, b.status]`
	if got := value[[]string](t, e.run(first)); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v", got)
	}
	oldA, oldB := str(allocationOf(e.lastRow("a", engine.EvPrepared)), "path"), str(allocationOf(e.lastRow("b", engine.EvPrepared)), "path")
	e.newProcess("proc-2")
	e.edits(nil)
	second := strings.Replace(first, "review one", "review one again", 1)
	if got := value[[]string](t, e.resume(second)); !slices.Equal(got, []string{"completed", "completed"}) {
		t.Fatalf("%v: a call that ran again in a suffix could not get its directory", got)
	}
	for label, old := range map[string]string{"a": oldA, "b": oldB} {
		now := str(allocationOf(e.lastRow(label, engine.EvPrepared)), "path")
		if now == "" || now == old {
			t.Errorf("call %s ran again in %q, its earlier attempt's directory %q", label, now, old)
		}
		for _, dir := range []string{old, now} {
			if _, err := os.Stat(dir); err != nil {
				t.Errorf("call %s: %v", label, err)
			}
		}
	}
	e.nothingLive(t, "proc-3", second, []string{"completed", "completed"})
}

// CRW-1269 d4: a call held after its first turn and goes on with an ordinary turn t2 is not in its repair when that turn is
// recovered: the process died after t2 started, t2 ended with an answer that does not satisfy the schema, and the next process
// sends the call's one repair turn t3 instead of ending the call null_model.
func TestHeldRetryAfterACrashIsNotMistakenForARepair(t *testing.T) {
	e := newEnv(t)
	e.edits(func(r fakehost.TurnRequest) *fakehost.TurnPlan {
		switch {
		case r.N == 1:
			return &fakehost.TurnPlan{Fail: &fakehost.Failure{Message: "busy", CodexErrorInfo: "serverOverloaded"}}
		case r.N == 2:
			return &fakehost.TurnPlan{Message: "this is not json"}
		}
		return &fakehost.TurnPlan{Message: `{"ok":true}`}
	})
	body := `const r = await agent.call('review the diff', { agentType: 'reviewer', label: 'rv', schema: { type: 'object', properties: { ok: { type: 'boolean' } }, required: ['ok'], additionalProperties: false } })
return [r.status, r.reason ?? '']`
	if got := value[[]string](t, e.run(body)); got[0] != "held" {
		t.Fatalf("%v", got)
	}
	callID := e.lastRow("rv", engine.EvHeld).CallID
	e.newProcess("proc-2")
	e.killWhen(func(ev engine.Event) bool {
		return ev.Kind == engine.KindCall && ev.Label == "rv" && ev.Event == engine.EvTurnStarted && ev.Data["k"] == 2
	})
	e.resume(body)
	e.newProcess("proc-3")
	if got := value[[]string](t, e.resume(body)); got[0] != "completed" {
		t.Fatalf("%v: the recovered ordinary turn was taken for the repair", got)
	}
	var markers []string
	for _, r := range e.requests("turn/start") {
		markers = append(markers, markerOf(turnText(r)))
	}
	if want := []string{callID + "/t1", callID + "/t2", callID + "/t3"}; !slices.Equal(markers, want) {
		t.Errorf("turns %v, want %v", markers, want)
	}
	if last := e.requests("turn/start"); len(last) > 0 && !strings.Contains(turnText(last[len(last)-1]), "did not satisfy the output schema") {
		t.Errorf("the last turn is not the repair: %q", turnText(last[len(last)-1]))
	}
	if done := e.lastRow("rv", engine.EvCompleted); done.Data["repair_used"] != true {
		t.Errorf("completed row %v: the repair was used", done.Data)
	}
}
