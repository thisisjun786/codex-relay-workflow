package cli

import (
	"bytes"
	"context"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1372 (2), Argon V5 G1 and G2 of fbed54e9: one party writes a session run's orchestrator line, the run's process, from its own
// started row; `crw flow next` reads the journal and the owner's file and writes neither the line nor a role_store_changed row. A next
// that resumes a dead run (G1), or that waits while another command resumes a paused one (G2), answers with what the resumed process
// recorded once it has recorded it, and the owner's line is that process's then. The processes are the real ones (`crw flow _run
// --resume`, `crw flow resume RUN`) over a world that holds them between the run lock and the started row.

// sessionTestPlacer places every call of the tests' session runs in a directory of its own.
type sessionTestPlacer struct{ *engine.Fake }

func (sessionTestPlacer) Place(req engine.PlaceRequest) (engine.Placement, error) {
	return engine.Placement{Kind: engine.PlaceCall, Path: "/w/call/" + req.CallID, Start: "base"}, nil
}
func (sessionTestPlacer) Establish(context.Context, engine.PlaceRequest, engine.Placement) error {
	return nil
}
func (sessionTestPlacer) Recheck(context.Context, engine.Placement) error  { return nil }
func (sessionTestPlacer) Snapshot(context.Context, string) (string, error) { return "T0", nil }

// idleSessionHost is a host on which no subagent was spawned: a published step waits for its dispatch report.
type idleSessionHost struct{}

func (idleSessionHost) ReadAgent(context.Context, string) (engine.SessionAgent, error) {
	return engine.SessionAgent{}, engine.ErrSessionAgentNotFound
}
func (idleSessionHost) Turns(context.Context, string, int, bool) ([]engine.SessionTurn, error) {
	return nil, nil
}
func (idleSessionHost) FindByMarker(context.Context, string, string) (string, bool, error) {
	return "", false, nil
}
func (idleSessionHost) Interrupt(context.Context, string, string) error { return nil }
func (idleSessionHost) Tokens(context.Context, string) (engine.Tokens, bool) {
	return engine.Tokens{}, false
}

// gatedProcesses runs the run processes of a test in this process, as the commands start them, over a session world that waits at a
// gate before it opens: a process holds the run lock and has written nothing of its own (no started row, no line) until the test
// opens the gate.
type gatedProcesses struct {
	t       *testing.T
	td      *testDeps
	gate    chan struct{}
	once    sync.Once
	mu      sync.Mutex
	cancels []context.CancelFunc
	wg      sync.WaitGroup
}

func newGatedProcesses(t *testing.T, td *testDeps) *gatedProcesses {
	t.Helper()
	g := &gatedProcesses{t: t, td: td, gate: make(chan struct{})}
	td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		select {
		case <-g.gate:
		case <-ctx.Done():
			return World{}, ctx.Err()
		}
		st, err := store.Open(ctx, filepath.Join(req.State, "relay.sqlite3"), "")
		if err != nil {
			return World{}, err
		}
		j, err := journal.Open(ctx, st, req.Spec.RunID, journal.Identity{}, journal.Options{})
		if err != nil {
			_ = st.Close()
			return World{}, err
		}
		return World{Journal: j, Backend: sessionTestPlacer{engine.NewFake()},
			Session: &engine.SessionConfig{Host: idleSessionHost{}, Bus: engine.NewFileStepBus(RunDir(req.State, req.Spec.RunID)), Owner: req.Spec.Thread,
				Poll: 20 * time.Millisecond, WakeAfter: time.Hour},
			Close: func() error { return errJoin(j.Close(), st.Close()) }}, nil
	}
	t.Cleanup(g.stop)
	return g
}

// run starts a command line of a run process in this process and returns at once.
func (g *gatedProcesses) run(args ...string) {
	ctx, cancel := context.WithCancel(context.Background())
	g.mu.Lock()
	g.cancels = append(g.cancels, cancel)
	g.mu.Unlock()
	g.wg.Add(1)
	go func() {
		defer g.wg.Done()
		var errOut bytes.Buffer
		if code := RunWith(ctx, g.td.Deps, args, strings.NewReader(""), &bytes.Buffer{}, &errOut); code != 0 && ctx.Err() == nil {
			g.t.Logf("%v: exit %d: %s", args, code, errOut.String())
		}
	}()
}

// openAfter opens the gate d after a process holds the run lock: next reads the run in that window.
func (g *gatedProcesses) openAfter(state, run string, d time.Duration) {
	go func() {
		for end := time.Now().Add(10 * time.Second); !LockHeld(state, run) && time.Now().Before(end); {
			time.Sleep(5 * time.Millisecond)
		}
		time.Sleep(d)
		g.open()
	}()
}

func (g *gatedProcesses) open() { g.once.Do(func() { close(g.gate) }) }

// stop ends the processes and waits for them.
func (g *gatedProcesses) stop() {
	g.open()
	g.mu.Lock()
	for _, cancel := range g.cancels {
		cancel()
	}
	g.mu.Unlock()
	g.wg.Wait()
}

// pinnedSessionTable is the table an earlier process of the run pinned: pairs the role store does not hold, under sha256:pinned.
func pinnedSessionTable() (*engine.Policy, map[string]any) {
	pairs, rows := map[string]engine.Pair{}, map[string]any{}
	for _, role := range bound.OrchestratorRoles {
		pairs[role] = engine.Pair{Model: "pinned-" + role, Effort: "high"}
		rows[role] = map[string]any{"model": "pinned-" + role, "effort": "high"}
	}
	return withPinnedPairs(engine.FakePolicy(), pairs, "sha256:pinned"), rows
}

// endedSessionProcess writes what the first process of a session run of the hello script left: its started row, which pinned the pairs
// of pinnedSessionTable (and names its process when token is set), and its open orchestrator line with those pairs. A dead process
// left the line open (it died without closing it); a paused one closed it as it ended. No process holds the run.
func endedSessionProcess(t *testing.T, w *world, run, owner, token string, paused bool) Spec {
	t.Helper()
	spec := Spec{Schema: SpecSchema, RunID: run, Workflow: "hello", Script: helloScript(t), Thread: owner, Backend: BackendSession, CreatedAt: "2026-10-14T00:00:00Z"}
	if err := WriteSpec(w.state, spec); err != nil {
		t.Fatal(err)
	}
	src, err := spec.Load()
	if err != nil {
		t.Fatal(err)
	}
	pinned, rows := pinnedSessionTable()
	j, err := journal.Open(ctx, w.st, run, journal.Identity{PlanID: "plan-1", PlanRevision: 3, ActorTaskID: owner, EpochNonce: "n"}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{"workflow": "hello", "script_digest": src.Program.Digest, "args_digest": argsDigest([]byte("null")), "backend": engine.BackendSession,
		"policy_digest": pinned.Digest, "role_pairs": rows}
	if token != "" {
		data["process"] = token
	}
	j.Append(engine.Event{RunID: run, Kind: engine.KindRun, Event: "started", Data: data})
	if paused {
		j.Append(engine.Event{RunID: run, Kind: engine.KindFlow, Event: engine.EvRunPaused, Data: map[string]any{"by": "operator", "reason": "crw flow pause"}})
	}
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if res, err := bindOwner(w.state, run, owner, pinned); err != nil || !res.Bound {
		t.Fatalf("the ended process's line: %+v %v", res, err)
	}
	if paused {
		if _, err := closeOwner(run, owner); err != nil {
			t.Fatal(err)
		}
	}
	return spec
}

// fastNextPoll makes next and dispatched look again every 20ms for the test.
func fastNextPoll(t *testing.T) {
	saved := nextPoll
	nextPoll = 20 * time.Millisecond
	t.Cleanup(func() { nextPoll = saved })
}

// lineOfTheStore says why the owner's line is not the run's open line with the role store's pairs ("" when it is).
func lineOfTheStore(got bound.Result, run string, table *engine.Policy) string {
	if got.State != bound.Orchestrator || got.Line.RunID != run || got.Line.PolicyDigest != table.Digest || !maps.Equal(got.Line.HelperPairs, ownerPairs(table)) {
		return fmt.Sprintf("the owner's line is %v %s %s, want run %s's with the pairs of %s", got.State, got.Line.RunID, got.Line.PolicyDigest, run, table.Digest)
	}
	return ""
}

// The earlier process of the run is one of a crw before the process token (its started row names none), or one of this crw.
var endedProcesses = []struct{ name, token string }{
	{"an earlier crw's process", ""},
	{"a process of this crw", "0123456789abcdef"},
}

// G1: next resumes a dead run after the role store changed. The process it starts holds the run and has recorded nothing yet when next
// reads the run; next answers once that process pinned the role store in its started row and bound the owner's line with it, and the
// line next answers beside is that process's, not the one the dead process left open with its pins.
func TestNextThatResumesADeadRunAnswersWithTheLineItsResumedProcessBound(t *testing.T) {
	for i, c := range endedProcesses {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fastNextPoll(t)
			w := newWorld(t)
			td := w.deps()
			run, owner := "run-0123456789c"+string(rune('1'+i)), "01a30000-0000-7000-8000-0000000000c1"
			endedSessionProcess(t, w, run, owner, c.token, false)
			if got := bound.Lookup(owner); got.State != bound.Orchestrator || got.Line.PolicyDigest != "sha256:pinned" {
				t.Fatalf("the dead process's line %+v", got)
			}
			storeTable, _, err := engine.LoadPolicy()
			if err != nil {
				t.Fatal(err)
			}
			g := newGatedProcesses(t, td)
			td.Spawn = func(ctx context.Context, req SpawnRequest) (int, error) {
				args := []string{ProcessCommand, "--state", req.State}
				if req.Resume {
					args = append(args, "--resume")
				}
				g.run(append(args, req.Spec.RunID)...)
				g.openAfter(req.State, req.Spec.RunID, 300*time.Millisecond)
				return os.Getpid(), nil
			}
			td.Ready = AwaitLock
			code, out, errOut := td.do("next", run, "--json", "--state", w.state)
			line := bound.Lookup(owner) // the owner's line when next answered
			if code != 0 {
				t.Fatalf("next: %d %s %s", code, out, errOut)
			}
			ans := decodeNext(t, out)
			if ans.Resumed == 0 {
				t.Fatalf("next did not resume the run: %+v", ans)
			}
			if why := lineOfTheStore(line, run, storeTable); why != "" {
				t.Fatalf("when next answered, %s: the line is the dead process's, not the one the process next resumed runs on (answer %+v)", why, ans)
			}
			noPinnedSpawns(t, ans, func(role string) string { return storeTable.Roles[role].Pair.Model })
			if n := roleStoreChangedRows(t, w, run); n != 0 {
				t.Fatalf("%d role_store_changed rows", n)
			}
		})
	}
}

// G2: next waits on a paused run, and another command (`crw flow resume RUN`) resumes it after the role store changed. next reads the
// run while the resuming process holds its lock and has not written its started row: it records no role_store_changed row from the
// ended process's started row, and it answers with the pairs the resuming process pinned and bound.
func TestNextThatWaitsWhileAnotherCommandResumesTheRunRecordsNoRoleStoreChange(t *testing.T) {
	for i, c := range endedProcesses {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			fastNextPoll(t)
			w := newWorld(t)
			td := w.deps()
			run, owner := "run-0123456789d"+string(rune('1'+i)), "01a30000-0000-7000-8000-0000000000c2"
			endedSessionProcess(t, w, run, owner, c.token, true)
			storeTable, _, err := engine.LoadPolicy()
			if err != nil {
				t.Fatal(err)
			}
			g := newGatedProcesses(t, td)
			go func() {
				time.Sleep(150 * time.Millisecond)
				g.run("resume", run, "--state", w.state)
				g.openAfter(w.state, run, 400*time.Millisecond)
			}()
			code, out, errOut := td.do("next", run, "--wait", "10", "--json", "--state", w.state)
			line := bound.Lookup(owner)
			if code != 0 {
				t.Fatalf("next: %d %s %s", code, out, errOut)
			}
			ans := decodeNext(t, out)
			if ans.Resumed != 0 || len(td.spawned) != 0 {
				t.Fatalf("next resumed a paused run: %+v", ans)
			}
			if n := roleStoreChangedRows(t, w, run); n != 0 {
				t.Fatalf("%d role_store_changed rows: next recorded the ended process's pins as the live process's (answer %+v)", n, ans)
			}
			noPinnedSpawns(t, ans, func(role string) string { return storeTable.Roles[role].Pair.Model })
			if why := lineOfTheStore(line, run, storeTable); why != "" {
				t.Fatalf("when next answered, %s", why)
			}
		})
	}
}

// The process lock: held while the process holds it, under the token its started row names; let go and its file removed when the
// process ends; a token that is not 16 lowercase hex digits names no file.
func TestProcessLockIsHeldUnderItsTokenUntilItIsLetGo(t *testing.T) {
	state, run := t.TempDir(), "run-0123456789e1"
	p, err := takeProcessLock(state, run)
	if err != nil {
		t.Fatal(err)
	}
	if !validProcessToken(p.Token()) || !processHeld(state, run, p.Token()) || !anyProcessHeld(state, run) {
		t.Fatalf("the process lock %q is not held", p.Token())
	}
	for _, bad := range []string{"", "../run.lock", "0123456789ABCDEF", "0123456789abcde"} {
		if processHeld(state, run, bad) {
			t.Errorf("token %q is held", bad)
		}
	}
	p.Release()
	if processHeld(state, run, p.Token()) || anyProcessHeld(state, run) {
		t.Fatal("the process lock is held after its release")
	}
	if _, err := os.Stat(processLockPath(state, run, p.Token())); !os.IsNotExist(err) {
		t.Fatalf("the process lock's file stays: %v", err)
	}
	var none *processLock
	none.Release()
	if none.Token() != "" {
		t.Fatal("no lock names a token")
	}
}

// A run whose start holds its start lock (its process does not hold the run yet) is starting: next resumes nothing, reads it again up
// to startingWait and answers starting with no spawn formats.
func TestNextTakesARunItsStartHoldsAsStarting(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	fastNextPoll(t)
	saved := startingWait
	startingWait = 200 * time.Millisecond
	t.Cleanup(func() { startingWait = saved })
	w := newWorld(t)
	td := w.deps()
	run := "run-0123456789e2"
	sessionSpec(t, w, run)
	start, err := takeStartLock(w.state, run)
	if err != nil {
		t.Fatal(err)
	}
	defer start.Release()
	code, out, errOut := td.do("next", run, "--json", "--state", w.state)
	if code != 0 {
		t.Fatalf("next: %d %s %s", code, out, errOut)
	}
	ans := decodeNext(t, out)
	if ans.State != NextStarting || ans.Resumed != 0 || len(td.spawned) != 0 || len(ans.Spawns) != 0 || !strings.Contains(ans.Hint, "is starting") {
		t.Fatalf("a run its start holds: %+v (spawned %d)", ans, len(td.spawned))
	}
}
