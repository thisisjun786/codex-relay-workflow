package cli

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const twoCalls = `export const meta = { name: 'two', phases: ['one', 'two'] }
phase('one')
const a = await agent.call('first', { agentType: 'executor', node: 'n1', label: 'first' })
phase('two')
const b = await agent.call('second', { agentType: 'executor', node: 'n1', label: 'second' })
return [a.status, b.status, b.reason]
`

// processWorld is a run process over the fake backend: the world it opens is the store of the test's state directory and the
// engine's fake host, which calls handle for each request.
type processWorld struct {
	*world
	td     *testDeps
	script string
	handle func(engine.FakeRequest) engine.FakeReply
	fake   *engine.Fake
}

func newProcessWorld(t *testing.T, script string) *processWorld {
	t.Helper()
	pw := &processWorld{world: newWorld(t)}
	pw.td = pw.world.deps()
	dir := t.TempDir()
	pw.script = filepath.Join(dir, "two.js")
	if err := os.WriteFile(pw.script, []byte(script), 0o644); err != nil {
		t.Fatal(err)
	}
	pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		st, err := store.Open(ctx, filepath.Join(req.State, "relay.sqlite3"), "")
		if err != nil {
			return World{}, err
		}
		id := journal.Identity{}
		if !req.Resume {
			id = journal.Identity{ActorTaskID: req.Spec.Thread, PlanID: "plan-1", PlanRevision: 3}
		}
		j, err := journal.Open(ctx, st, req.Spec.RunID, id, journal.Options{})
		if err != nil {
			return World{}, err
		}
		// one host for the processes of a test: a thread a process started is there for the process that resumes it
		if pw.fake == nil {
			pw.fake = engine.NewFake()
		}
		fake := pw.fake
		fake.Handler = pw.handle
		return World{Journal: j, Backend: fake, Close: func() error { return errJoin(j.Close(), st.Close()) }}, nil
	}
	return pw
}

func errJoin(errs ...error) error {
	for _, e := range errs {
		if e != nil {
			return e
		}
	}
	return nil
}

// spec writes the spec of a run of the script and returns it.
func (pw *processWorld) spec(id string, mutate func(*Spec)) Spec {
	pw.t.Helper()
	s := Spec{Schema: SpecSchema, RunID: id, Workflow: "two", Script: pw.script, Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z"}
	if mutate != nil {
		mutate(&s)
	}
	if err := WriteSpec(pw.state, s); err != nil {
		pw.t.Fatal(err)
	}
	return s
}

// execute runs the process of a run in this test, with the signals it is sent.
func (pw *processWorld) execute(spec Spec, resume bool, sigs chan os.Signal) (int, string) {
	pw.t.Helper()
	pw.world.close()
	defer pw.world.open()
	var out, errOut bytes.Buffer
	c := &command{Deps: pw.td.Deps, ctx: ctx, stdout: &out, stderr: &errOut, name: ProcessCommand}
	code := c.execute(spec, pw.state, resume, sigs)
	return code, errOut.String()
}

func (pw *processWorld) info(id string) RunInfo {
	pw.t.Helper()
	r, err := OpenReader(ctx, pw.state, "")
	if err != nil {
		pw.t.Fatal(err)
	}
	defer r.Close()
	rec, err := r.Record(ctx, id)
	if err != nil {
		pw.t.Fatal(err)
	}
	info, err := r.Info(ctx, rec)
	if err != nil {
		pw.t.Fatal(err)
	}
	return info
}

// TestRunProcessHoldsTheLockAndLeavesTheResult: the process of a run holds the run lock for as long as it works (a second process
// is refused, ps shows the run running), writes the run's rows under the identity of its spec (the owner thread), leaves the
// value its script returned in result.json, and releases the lock when it ends: the run is then completed.
func TestRunProcessHoldsTheLockAndLeavesTheResult(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	spec := pw.spec("run-p1", nil)
	var during RunInfo
	var second int
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if r.Kind == "turn" && strings.Contains(r.Prompt, "second") {
			during = pw.info("run-p1")
			pw.world.close()
			c := &command{Deps: pw.td.Deps, ctx: ctx, stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}, name: ProcessCommand}
			second = c.execute(spec, pw.state, false, nil)
			pw.world.open()
		}
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	code, errOut := pw.execute(spec, false, nil)
	if code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if second != exitLive {
		t.Errorf("a second process of the run exits %d, want %d", second, exitLive)
	}
	if during.State != StateRunning || !during.Live || during.PID != os.Getpid() || during.Owner != "thread-owner" {
		t.Errorf("the run while its process works: %+v", during)
	}
	if LockHeld(pw.state, "run-p1") {
		t.Error("the lock is held after the process ended")
	}
	info := pw.info("run-p1")
	if info.State != StateCompleted || info.Calls != 2 || info.Tokens != 30 || info.Processes != 1 || info.Owner != "thread-owner" || info.PlanID != "plan-1" {
		t.Errorf("the run after: %+v", info)
	}
	res, ok := ReadResult(pw.state, "run-p1")
	if !ok || res.Status != engine.RunCompleted || string(compactJSON(res.Value)) != `["completed","completed",null]` || res.Tokens != 30 {
		t.Errorf("the result: %+v %v", res, ok)
	}
}

// TestPauseAndCancelSignalTheRunProcess: SIGUSR1 pauses the process (the call in flight is interrupted and the calls that would send end held engine_paused, with one
// run_paused row; the process exits 0 and the run ends paused: a run/paused row and no finished row, nothing told finished, and a
// resume goes on), SIGTERM ends it (the run is cancelled), a deadline that passed pauses the run by itself (it finishes held), and
// `crw flow pause` and `crw flow cancel` send those signals to the process the lock names.
func TestPauseAndCancelSignalTheRunProcess(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		hook := &upwardRecorder{}
		inner := pw.td.World
		pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
			w, err := inner(ctx, req)
			w.Upward = hook
			return w, err
		}
		spec := pw.spec("run-pause", nil)
		sigs := make(chan os.Signal, 2)
		pause := true
		pw.handle = func(r engine.FakeRequest) engine.FakeReply {
			if pause && r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
				sigs <- syscall.SIGUSR1
				time.Sleep(200 * time.Millisecond) // the signal is read before the second call is sent
			}
			return engine.FakeReply{}
		}
		if code, errOut := pw.execute(spec, false, sigs); code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		info := pw.info("run-pause")
		if info.State != StatePaused || info.Live || info.Status != engine.RunPaused || info.Paused != "operator: crw flow pause" || len(info.HeldReasons) != 1 || info.HeldReasons[0] != engine.ReasonEnginePaused || info.Unsettled != 2 {
			t.Errorf("the paused run: %+v", info)
		}
		events, _, err := loadRun(pw.world, "run-pause")
		if err != nil {
			t.Fatal(err)
		}
		if ends := endRowsAfterLastStart(events); strings.Join(ends, ",") != "paused" {
			t.Errorf("the run rows after the last start: %v, want one paused row", ends)
		}
		if got := strings.Join(hook.calls, "|"); got != "started run-pause" {
			t.Errorf("the hook was called %q: a paused run is not told finished", got)
		}
		res, _ := ReadResult(pw.state, "run-pause")
		if res.Status != engine.RunPaused || string(compactJSON(res.Value)) != `["held","held","engine_paused"]` {
			t.Errorf("the result: %+v %s", res, res.Value)
		}
		// a resume goes on to the end, and that end is told
		pause = false
		if code, errOut := pw.execute(spec, true, nil); code != 0 {
			t.Fatalf("the resume: exit %d %s", code, errOut)
		}
		if info := pw.info("run-pause"); info.State != StateCompleted || info.Processes != 2 {
			t.Errorf("the resumed run: %+v", info)
		}
		if got := strings.Join(hook.calls, "|"); got != "started run-pause|started run-pause|finished run-pause completed" {
			t.Errorf("the hook was called %q", got)
		}
	})
	t.Run("cancel", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		spec := pw.spec("run-cancel", nil)
		sigs := make(chan os.Signal, 2)
		pw.handle = func(r engine.FakeRequest) engine.FakeReply {
			if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
				sigs <- syscall.SIGTERM
				time.Sleep(200 * time.Millisecond)
			}
			return engine.FakeReply{}
		}
		if code, _ := pw.execute(spec, false, sigs); code != 1 {
			t.Fatalf("a cancelled run exits %d", code)
		}
		if info := pw.info("run-cancel"); info.State != StateCancelled || info.ErrorKind != engine.ErrKindCancelled {
			t.Errorf("the cancelled run: %+v", info)
		}
	})
	t.Run("deadline", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		spec := pw.spec("run-late", func(s *Spec) { s.Deadline = "2026-10-12T08:59:00Z" })
		if code, errOut := pw.execute(spec, false, nil); code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		info := pw.info("run-late")
		if info.State != StateHeld || !strings.HasPrefix(info.Paused, "deadline: ") || info.Calls != 2 {
			t.Errorf("the run past its deadline: %+v", info)
		}
	})
	t.Run("commands", func(t *testing.T) {
		w := newWorld(t)
		w.newRun("run-sig", "thread-owner").close()
		td := w.deps()
		var sent []syscall.Signal
		td.Signal = func(pid int, sig syscall.Signal) error {
			if pid != os.Getpid() {
				t.Errorf("signalled process %d, the lock names %d", pid, os.Getpid())
			}
			sent = append(sent, sig)
			return nil
		}
		// no process holds the run: nothing to pause, and a cancel finishes it in the journal
		if code, _, errOut := td.do("pause", "run-sig", "--state", w.state); code != 1 || !strings.Contains(errOut, "no process holds run run-sig") {
			t.Errorf("pause of a dead run: %d %q", code, errOut)
		}
		lock, err := TakeLock(w.state, "run-sig", "test", td.Now())
		if err != nil {
			t.Fatal(err)
		}
		if code, out, _ := td.do("pause", "run-sig", "--state", w.state); code != 0 || !strings.Contains(out, "pause requested of run run-sig") {
			t.Errorf("pause: %d %q", code, out)
		}
		go func() { time.Sleep(100 * time.Millisecond); lock.Release() }()
		if code, out, errOut := td.do("cancel", "run-sig", "--state", w.state); code != 0 || !strings.Contains(out, "run run-sig ended") {
			t.Errorf("cancel: %d %q %q", code, out, errOut)
		}
		if len(sent) != 2 || sent[0] != syscall.SIGUSR1 || sent[1] != syscall.SIGTERM {
			t.Errorf("signals %v", sent)
		}
		// a dead run is cancelled in the journal, once
		w.newRun("run-dead", "thread-owner").close()
		if code, out, errOut := td.do("cancel", "run-dead", "--state", w.state); code != 0 || !strings.Contains(out, "had no process; it is cancelled") {
			t.Errorf("cancel of a dead run: %d %q %q", code, out, errOut)
		}
		_, out, _ := td.do("show", "run-dead", "--state", w.state, "--json")
		if !strings.Contains(out, `"state": "cancelled"`) {
			t.Errorf("show: %s", out)
		}
		if code, _, errOut := td.do("cancel", "run-dead", "--state", w.state); code != 1 || !strings.Contains(errOut, "nothing to cancel") {
			t.Errorf("a second cancel: %d %q", code, errOut)
		}
	})
}

// TestResumeTakesTheRunFromItsSpec: a run that was paused is taken again by `crw flow resume RUN` with the workflow and the
// arguments of its spec, once its lock is free (a held lock refuses it); --detach starts the process instead of running it, and a
// run that no `crw flow start` made has no spec to resume from.
func TestResumeTakesTheRunFromItsSpec(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	spec := pw.spec("run-again", nil)
	sigs := make(chan os.Signal, 2)
	paused := true
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if paused && r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
			sigs <- syscall.SIGUSR1
			time.Sleep(200 * time.Millisecond)
		}
		return engine.FakeReply{}
	}
	if code, errOut := pw.execute(spec, false, sigs); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if info := pw.info("run-again"); info.State != StatePaused {
		t.Fatalf("the paused run: %+v", info)
	}
	paused = false

	// a live run is not resumed
	lock, err := TakeLock(pw.state, "run-again", "test", pw.td.Now())
	if err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := pw.td.do("resume", "run-again", "--state", pw.state); code != exitLive || !strings.Contains(errOut, "a process holds the run run-again") {
		t.Errorf("resume of a live run: %d %q", code, errOut)
	}
	if code, _, errOut := pw.td.do("resume", "run-again", "--state", pw.state, "--detach"); code != 2 || !strings.Contains(errOut, "a process holds the run") || len(pw.td.spawned) != 0 {
		t.Errorf("--detach of a live run: %d %q, %d processes", code, errOut, len(pw.td.spawned))
	}
	lock.Release()

	if code, out, errOut := pw.td.do("resume", "run-again", "--state", pw.state, "--detach"); code != 0 || !strings.Contains(out, "run run-again resumed (process 4242)") || len(pw.td.spawned) != 1 || !pw.td.spawned[0].Resume || pw.td.spawned[0].Spec.Script != pw.script {
		t.Fatalf("--detach: %d %q %q %+v", code, out, errOut, pw.td.spawned)
	}
	if code, out, errOut := pw.td.do("resume", "run-again", "--state", pw.state); code != 0 {
		t.Fatalf("resume: exit %d\n%s\n%s", code, out, errOut)
	}
	info := pw.info("run-again")
	if info.State != StateCompleted || info.Processes != 2 {
		t.Errorf("the resumed run: %+v", info)
	}
	if code, _, errOut := pw.td.do("resume", "run-nospec", "--state", pw.state); code != 2 || !strings.Contains(errOut, "has no spec file") {
		t.Errorf("a run without a spec: %d %q", code, errOut)
	}
}

// upwardRecorder is an engine.UpwardHook that records the calls it is given.
type upwardRecorder struct {
	mu    sync.Mutex
	calls []string
}

func (h *upwardRecorder) note(s string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.calls = append(h.calls, s)
}

func (h *upwardRecorder) RunStarted(_ context.Context, run string) error {
	h.note("started " + run)
	return nil
}

func (h *upwardRecorder) BatchCommitted(_ context.Context, run string) error { return nil }

func (h *upwardRecorder) RunFinished(_ context.Context, run, status string) error {
	h.note("finished " + run + " " + status)
	return nil
}

// The run process hands the run to the upward hook of its world (CRW-1204's journal.UpwardFactory, which the binary installs),
// as the engine's own resume command does: when it starts, and when it finishes with the status the run came to - a fresh run
// and one the process resumes alike.
func TestRunProcessHandsTheRunToTheUpwardHook(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	hook := &upwardRecorder{}
	inner := pw.td.World
	pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
		w, err := inner(ctx, req)
		w.Upward = hook
		return w, err
	}
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	spec := pw.spec("run-up", nil)
	if code, errOut := pw.execute(spec, false, nil); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	if code, errOut := pw.execute(spec, true, nil); code != 0 {
		t.Fatalf("the resumed process: exit %d %s", code, errOut)
	}
	want := "started run-up|finished run-up completed|started run-up|finished run-up completed"
	if got := strings.Join(hook.calls, "|"); got != want {
		t.Errorf("the hook was called %q, want %q", got, want)
	}
}

// ProductionWorld installs the upward hook journal.UpwardFactory makes (the binary installs the factory, internal/flow/cli
// UpwardFactory) over the run's store and journal, with the state directory the run is in and the socket of its spec; without a
// factory it installs none.
func TestProductionWorldInstallsTheUpwardHook(t *testing.T) {
	w := newWorld(t)
	w.close()
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY", "")
	t.Setenv("CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST", "")
	socket := filepath.Join(t.TempDir(), "app.sock")
	spec := Spec{Schema: SpecSchema, RunID: "run-pw", Workflow: "two", Thread: "thread-owner", Repo: repo, Socket: socket}
	hook := &upwardRecorder{}
	var got []string
	prev := journal.UpwardFactory
	t.Cleanup(func() { journal.UpwardFactory = prev })
	journal.UpwardFactory = func(st *store.Store, j *journal.Journal, runID, state, sock string) engine.UpwardHook {
		got = append(got, runID, state, sock)
		if st == nil || j == nil {
			t.Error("the factory is given no store or journal")
		}
		return hook
	}
	world, err := ProductionWorld(ctx, WorldRequest{Spec: spec, State: w.state})
	if err != nil {
		t.Fatal(err)
	}
	upward := world.Upward
	if err := world.Close(); err != nil {
		t.Fatal(err)
	}
	if upward != hook || strings.Join(got, "|") != "run-pw|"+w.state+"|"+socket {
		t.Errorf("the world's hook is %v, the factory was called with %q", upward, got)
	}
	journal.UpwardFactory = nil
	world, err = ProductionWorld(ctx, WorldRequest{Spec: spec, State: w.state, Resume: true})
	if err != nil {
		t.Fatal(err)
	}
	upward = world.Upward
	if err := world.Close(); err != nil {
		t.Fatal(err)
	}
	if upward != nil {
		t.Errorf("without a factory the world's hook is %v", upward)
	}
}

// TestForegroundResumePausesCleanlyOnSIGUSR1 (CRW-1278): `crw flow pause RUN` sends SIGUSR1 to the process that holds the run lock,
// and a foreground `crw flow resume RUN SCRIPT` (and `crw flow resume RUN`) answers it as the detached run process does: the run
// writes run_paused, its calls end held engine_paused, the process is not killed and returns, and a later resume goes on.
func TestForegroundResumePausesCleanlyOnSIGUSR1(t *testing.T) {
	for _, path := range []string{"script", "spec"} {
		t.Run(path, func(t *testing.T) {
			pw := newProcessWorld(t, twoCalls)
			hook := &upwardRecorder{}
			inner := pw.td.World
			pw.td.World = func(ctx context.Context, req WorldRequest) (World, error) {
				w, err := inner(ctx, req)
				w.Upward = hook
				return w, err
			}
			scriptResumeEnv(t, pw)
			run := "run-usr1-" + path
			resume := func() (int, string, string) {
				args := []string{"resume", run}
				if path == "script" {
					args = append(args, pw.script)
				}
				return pw.td.do(append(args, "--state", pw.state)...)
			}
			// the first process is paused in its first call
			sigs := make(chan os.Signal, 2)
			mode := "inject"
			pw.handle = func(r engine.FakeRequest) engine.FakeReply {
				if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
					switch mode {
					case "inject":
						sigs <- syscall.SIGUSR1
						time.Sleep(200 * time.Millisecond)
					case "signal":
						// what `crw flow pause` does to the lock holder: this process
						if err := syscall.Kill(os.Getpid(), syscall.SIGUSR1); err != nil {
							t.Error(err)
						}
						time.Sleep(200 * time.Millisecond)
					}
				}
				return engine.FakeReply{}
			}
			spec := pw.spec(run, nil)
			if code, errOut := pw.execute(spec, false, sigs); code != 0 {
				t.Fatalf("the first process: exit %d %s", code, errOut)
			}
			// the foreground resume is paused by a SIGUSR1 sent to its process
			mode = "signal"
			code, out, errOut := resume()
			if code != 0 {
				t.Fatalf("the paused resume: exit %d %s %s", code, out, errOut)
			}
			info := pw.info(run)
			if info.Processes != 2 || info.Live || info.State != StatePaused || info.Status != engine.RunPaused || info.Paused != "operator: crw flow pause" || len(info.HeldReasons) != 1 || info.HeldReasons[0] != engine.ReasonEnginePaused {
				t.Errorf("the resume did not pause cleanly: %+v", info)
			}
			events, _, err := loadRun(pw.world, run)
			if err != nil {
				t.Fatal(err)
			}
			if ends := endRowsAfterLastStart(events); strings.Join(ends, ",") != "paused" {
				t.Errorf("the run rows after the last start: %v, want one paused row", ends)
			}
			if got := strings.Join(hook.calls, "|"); got != "started "+run+"|started "+run {
				t.Errorf("the hook was called %q: a paused run is not told finished", got)
			}
			// the end the resume came to: the engine's result on stdout (SCRIPT) or result.json (the spec's process)
			if path == "script" {
				if !strings.Contains(out, `"status": "paused"`) {
					t.Errorf("the result of the paused resume: %s", out)
				}
			} else if res, _ := ReadResult(pw.state, run); res.Status != engine.RunPaused {
				t.Errorf("the result of the paused resume: %+v", res)
			}
			pauses := 0
			for _, e := range events {
				if e.Kind == engine.KindFlow && e.Event == engine.EvRunPaused && e.Data["by"] == "operator" {
					pauses++
				}
			}
			if pauses != 2 {
				t.Errorf("%d run_paused rows of the operator, want one for each process", pauses)
			}
			// a later resume goes on to the end
			mode = ""
			if code, out, errOut := resume(); code != 0 {
				t.Fatalf("the last resume: exit %d %s %s", code, out, errOut)
			}
			if info := pw.info(run); info.State != StateCompleted || info.Processes != 3 {
				t.Errorf("the run after the pause: %+v", info)
			}
		})
	}
}

// drainedRows counts, after the last start of the run, the interrupt_requested rows and the run_paused rows by answer.
func drainedRows(events []engine.Event) (interrupts, drains int) {
	last := -1
	for i, e := range events {
		if e.Kind == engine.KindRun && e.Event == "started" {
			last = i
		}
	}
	for _, e := range events[last+1:] {
		switch {
		case e.Event == engine.EvInterruptRequested:
			interrupts++
		case e.Kind == engine.KindFlow && e.Event == engine.EvRunPaused && e.Data["by"] == "answer":
			drains++
		}
	}
	return interrupts, drains
}

// TestRunProcessDrainsOnSIGUSR2 (CRW-1492): `crw flow answer` sends SIGUSR2 to the run process that holds the run; the process
// drains the run (Control.Drain): the turn in flight runs to its end with its tokens, the next call ends held engine_paused, nothing
// is interrupted, and the run ends paused by `answer` with the process exiting 0 (not cancelled, as any other signal would).
func TestRunProcessDrainsOnSIGUSR2(t *testing.T) {
	pw := newProcessWorld(t, twoCalls)
	spec := pw.spec("run-drain", nil)
	sigs := make(chan os.Signal, 2)
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
			sigs <- syscall.SIGUSR2
			time.Sleep(200 * time.Millisecond) // the signal is read while the turn is in flight
		}
		return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
	}
	if code, errOut := pw.execute(spec, false, sigs); code != 0 {
		t.Fatalf("exit %d %s", code, errOut)
	}
	info := pw.info("run-drain")
	if info.State != StatePaused || info.Live || info.Status != engine.RunPaused || !strings.HasPrefix(info.Paused, "answer: ") || len(info.HeldReasons) != 1 || info.HeldReasons[0] != engine.ReasonEnginePaused {
		t.Errorf("the drained run: %+v", info)
	}
	res, _ := ReadResult(pw.state, "run-drain")
	if res.Status != engine.RunPaused || string(compactJSON(res.Value)) != `["completed","held","engine_paused"]` || res.Tokens != 15 {
		t.Errorf("the result: %+v %s", res, res.Value)
	}
	events, _, err := loadRun(pw.world, "run-drain")
	if err != nil {
		t.Fatal(err)
	}
	if ends := endRowsAfterLastStart(events); strings.Join(ends, ",") != "paused" {
		t.Errorf("the run rows after the last start: %v, want one paused row", ends)
	}
	if interrupts, drains := drainedRows(events); interrupts != 0 || drains != 1 {
		t.Errorf("%d interrupt_requested rows and %d run_paused rows by answer, want 0 and 1", interrupts, drains)
	}
}

// TestForegroundResumeDrainsOnSIGUSR2 (CRW-1492): a foreground `crw flow resume RUN SCRIPT` and `crw flow resume RUN` hold the run as
// its process does and answer SIGUSR2 as it does: the run ends paused by `answer`, nothing is interrupted, and the command exits 0.
// The SCRIPT form takes SIGUSR2 from before its holder record exists: a signal sent the moment it holds the run is not lost.
func TestForegroundResumeDrainsOnSIGUSR2(t *testing.T) {
	for _, path := range []string{"script", "spec"} {
		t.Run(path, func(t *testing.T) {
			pw := newProcessWorld(t, twoCalls)
			scriptResumeEnv(t, pw)
			run := "run-usr2-" + path
			// the first process is paused by the operator in its first call
			sigs := make(chan os.Signal, 2)
			mode := "pause"
			pw.handle = func(r engine.FakeRequest) engine.FakeReply {
				if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
					switch mode {
					case "pause":
						sigs <- syscall.SIGUSR1
						time.Sleep(200 * time.Millisecond)
					case "signal":
						// what `crw flow answer` does to the run process that holds the run: this process
						if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
							t.Error(err)
						}
						time.Sleep(200 * time.Millisecond)
					}
				}
				return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
			}
			spec := pw.spec(run, nil)
			if code, errOut := pw.execute(spec, false, sigs); code != 0 {
				t.Fatalf("the first process: exit %d %s", code, errOut)
			}
			args := []string{"resume", run}
			if path == "script" {
				args = append(args, pw.script)
				// the signal comes the moment the resume holds the run, before its first call
				mode = ""
				prev := afterResumeLock
				t.Cleanup(func() { afterResumeLock = prev })
				afterResumeLock = func() {
					if err := syscall.Kill(os.Getpid(), syscall.SIGUSR2); err != nil {
						t.Error(err)
					}
					time.Sleep(200 * time.Millisecond)
				}
			} else {
				mode = "signal"
			}
			code, out, errOut := pw.td.do(append(args, "--state", pw.state)...)
			if code != 0 {
				t.Fatalf("the drained resume: exit %d %s %s", code, out, errOut)
			}
			info := pw.info(run)
			if info.Processes != 2 || info.Live || info.State != StatePaused || info.Status != engine.RunPaused || !strings.HasPrefix(info.Paused, "answer: ") {
				t.Errorf("the resume did not drain: %+v", info)
			}
			events, _, err := loadRun(pw.world, run)
			if err != nil {
				t.Fatal(err)
			}
			if ends := endRowsAfterLastStart(events); strings.Join(ends, ",") != "paused" {
				t.Errorf("the run rows after the last start: %v, want one paused row", ends)
			}
			if interrupts, drains := drainedRows(events); interrupts != 0 || drains != 1 {
				t.Errorf("%d interrupt_requested rows and %d run_paused rows by answer after the last start, want 0 and 1", interrupts, drains)
			}
			if path == "spec" {
				// the turn in flight ended with its tokens; the next call is held
				if res, _ := ReadResult(pw.state, run); res.Status != engine.RunPaused || string(compactJSON(res.Value)) != `["completed","held","engine_paused"]` {
					t.Errorf("the result of the drained resume: %+v %s", res, res.Value)
				}
			} else if !strings.Contains(out, `"status": "paused"`) {
				t.Errorf("the result of the drained resume: %s", out)
			}
		})
	}
}
