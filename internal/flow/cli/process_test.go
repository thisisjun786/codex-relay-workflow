package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
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
		fake := engine.NewFake()
		if pw.handle != nil {
			fake.Handler = pw.handle
		}
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
// run_paused row, and the run is held, not over), SIGTERM ends it (the run is cancelled), a deadline that passed pauses the run
// by itself, and `crw flow pause` and `crw flow cancel` send those signals to the process the lock names.
func TestPauseAndCancelSignalTheRunProcess(t *testing.T) {
	t.Run("pause", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		spec := pw.spec("run-pause", nil)
		sigs := make(chan os.Signal, 2)
		pw.handle = func(r engine.FakeRequest) engine.FakeReply {
			if r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
				sigs <- syscall.SIGUSR1
				time.Sleep(200 * time.Millisecond) // the signal is read before the second call is sent
			}
			return engine.FakeReply{}
		}
		if code, errOut := pw.execute(spec, false, sigs); code != 0 {
			t.Fatalf("exit %d %s", code, errOut)
		}
		info := pw.info("run-pause")
		if info.State != StateHeld || info.Paused != "operator: crw flow pause" || len(info.HeldReasons) != 1 || info.HeldReasons[0] != engine.ReasonEnginePaused || info.Unsettled != 2 {
			t.Errorf("the paused run: %+v", info)
		}
		res, _ := ReadResult(pw.state, "run-pause")
		if string(compactJSON(res.Value)) != `["held","held","engine_paused"]` {
			t.Errorf("the value: %s", res.Value)
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
