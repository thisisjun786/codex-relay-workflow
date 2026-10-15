package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// writerCommands are the commands that take a free run lock to append rows and run no script (CRW-1495 item 6).
var writerCommands = []string{"audit", "answer", "cancel", "recover"}

// holdLock takes the run lock of run as command would, for the test; the caller releases it.
func holdLock(t *testing.T, w *world, run, command string) *Lock {
	t.Helper()
	l, err := TakeLock(w.state, run, command, time.Now())
	if err != nil {
		t.Fatalf("the lock of %s as %s: %v", run, command, err)
	}
	return l
}

// readRun reads a run as `ps --all --json` and `show --json` give it.
func readRun(t *testing.T, td *testDeps, run string) (RunInfo, RunDetail) {
	t.Helper()
	code, out, errOut := td.do("ps", "--all", "--json", "--state", td.w.state)
	var ps struct {
		Runs []RunInfo `json:"runs"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &ps) != nil {
		t.Fatalf("ps --all --json: %d %q %q", code, out, errOut)
	}
	var info *RunInfo
	for i := range ps.Runs {
		if ps.Runs[i].RunID == run {
			info = &ps.Runs[i]
		}
	}
	if info == nil {
		t.Fatalf("ps --all --json has no run %s: %s", run, out)
	}
	code, out, errOut = td.do("show", run, "--state", td.w.state, "--json")
	var d RunDetail
	if code != 0 || json.Unmarshal([]byte(out), &d) != nil {
		t.Fatalf("show --json: %d %q %q", code, out, errOut)
	}
	return *info, d
}

// TestAWriterHoldIsNotARunProcess (CRW-1495 item 6): a command that holds the run lock to write rows and runs no script (audit,
// answer, cancel, recover) is not a process of the run: ps and show read the run as its rows say, with no live process and no pid,
// and pause signals nothing. A hold of the run process (_run) is read as before, and a released lock keeps no holder record.
func TestAWriterHoldIsNotARunProcess(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-dead", "thread-owner").close()
	p := w.newRun("run-paused", "thread-owner")
	p.endPaused()
	p.close()
	td := w.deps()
	var sent []int
	td.Signal = func(pid int, sig syscall.Signal) error { sent = append(sent, pid); return nil }
	for run, state := range map[string]string{"run-dead": StateDead, "run-paused": StatePaused} {
		for _, cmd := range writerCommands {
			t.Run(run+"/"+cmd, func(t *testing.T) {
				sent = nil
				l := holdLock(t, w, run, cmd)
				defer l.Release()
				if live, pid, command := runHolder(w.state, run); live || pid != 0 || command != cmd {
					t.Errorf("runHolder: (%v, %d, %q), want (false, 0, %q)", live, pid, command, cmd)
				}
				info, d := readRun(t, td, run)
				if info.Live || info.PID != 0 || info.State != state {
					t.Errorf("ps: live %v pid %d state %q, want not live, pid 0, %s", info.Live, info.PID, info.State, state)
				}
				if d.Live || d.PID != 0 || d.State != state {
					t.Errorf("show: live %v pid %d state %q, want not live, pid 0, %s", d.Live, d.PID, d.State, state)
				}
				code, out, errOut := td.do("pause", run, "--state", w.state)
				if code != 1 || !strings.Contains(errOut, "no process holds") || len(sent) != 0 {
					t.Errorf("pause: exit %d, signals to %v, out %q, err %q", code, sent, out, errOut)
				}
			})
		}
	}
	t.Run("run process", func(t *testing.T) {
		l := holdLock(t, w, "run-dead", "_run")
		if live, pid, command := runHolder(w.state, "run-dead"); !live || pid != os.Getpid() || command != "_run" {
			t.Errorf("runHolder: (%v, %d, %q), want (true, %d, _run)", live, pid, command, os.Getpid())
		}
		info, d := readRun(t, td, "run-dead")
		if !info.Live || info.PID != os.Getpid() || !d.Live || d.PID != os.Getpid() {
			t.Errorf("ps: live %v pid %d; show: live %v pid %d; want live, pid %d", info.Live, info.PID, d.Live, d.PID, os.Getpid())
		}
		l.Release()
		raw, err := os.ReadFile(lockPath(w.state, "run-dead"))
		if err != nil || len(raw) != 0 {
			t.Errorf("the released lock file: %q %v, want empty", raw, err)
		}
		if live, pid, _ := runHolder(w.state, "run-dead"); live || pid != 0 {
			t.Errorf("runHolder of a free lock: (%v, %d)", live, pid)
		}
	})
}

// TestTailKeepsAWriterHoldLive (guard): Tail is the commit barrier of watch and the GUI stream, so a writer's hold reads live there,
// and a follower does not stop before the writer's row is stored.
func TestTailKeepsAWriterHoldLive(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-dead", "thread-owner").close()
	l := holdLock(t, w, "run-dead", "audit")
	defer l.Release()
	rd, err := OpenReader(ctx, w.state, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rd.Close()
	page, err := rd.Tail(ctx, "run-dead", 0, 10, 0)
	if err != nil || !page.Live {
		t.Fatalf("Tail during an audit hold: live %v err %v", page.Live, err)
	}
}

// TestAwaitLockIsNotAWritersHold (CRW-1495 item 6): a writer that holds the run lock right after a spawn is not the spawned process
// holding the run: AwaitLock reports a process that ended without holding it.
func TestAwaitLockIsNotAWritersHold(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-dead", "thread-owner").close()
	l := holdLock(t, w, "run-dead", "audit")
	defer l.Release()
	gone := exec.Command("true")
	since := time.Now()
	if err := gone.Run(); err != nil {
		t.Fatal(err)
	}
	err := AwaitLock(ctx, w.state, "run-dead", gone.Process.Pid, since)
	if err == nil || !strings.Contains(err.Error(), "ended before it held the run") {
		t.Fatalf("AwaitLock during an audit hold: %v", err)
	}
}

// TestTakeLockPastWritersWaitsAWriterOut (CRW-1495 item 6): takeLockPastWriters waits a writer's hold out within its wait, gives up
// on a writer that holds past it, and refuses a run process's hold at once.
func TestTakeLockPastWritersWaitsAWriterOut(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-dead", "thread-owner").close()
	held := func(command string, hold time.Duration) chan struct{} {
		l := holdLock(t, w, "run-dead", command)
		done := make(chan struct{})
		go func() { time.Sleep(hold); l.Release(); close(done) }()
		return done
	}
	t.Run("a writer that lets go", func(t *testing.T) {
		done := held("audit", 150*time.Millisecond)
		l, err := takeLockPastWriters(w.state, "run-dead", "cancel", time.Now(), writerWait)
		if err != nil {
			t.Fatalf("after an audit hold of 150ms, a wait of %s: %v", writerWait, err)
		}
		l.Release()
		<-done
	})
	t.Run("a writer past the wait", func(t *testing.T) {
		done := held("audit", 500*time.Millisecond)
		defer func() { <-done }()
		if l, err := takeLockPastWriters(w.state, "run-dead", "cancel", time.Now(), 50*time.Millisecond); !errors.Is(err, ErrRunLive) {
			l.Release()
			t.Fatalf("an audit hold of 500ms with a wait of 50ms: %v", err)
		}
	})
	t.Run("a run process", func(t *testing.T) {
		done := held("_run", 1500*time.Millisecond)
		defer func() { <-done }()
		began := time.Now()
		if l, err := takeLockPastWriters(w.state, "run-dead", "cancel", time.Now(), 5*time.Second); !errors.Is(err, ErrRunLive) || time.Since(began) > time.Second {
			l.Release()
			t.Fatalf("a _run hold: %v after %s", err, time.Since(began))
		}
	})
}

// releaseAfter holds the run lock of run as command, and lets it go after hold on a goroutine of its own, which touches only that
// lock; the channel closes once it is let go.
func releaseAfter(t *testing.T, w *world, run, command string, hold time.Duration) chan struct{} {
	t.Helper()
	l := holdLock(t, w, run, command)
	done := make(chan struct{})
	go func() { time.Sleep(hold); l.Release(); close(done) }()
	return done
}

// runRows are the rows of run whose event is event, read from the world's store.
func runRows(t *testing.T, w *world, run, event string) []engine.Event {
	t.Helper()
	rows, _, err := loadRun(w, run)
	if err != nil {
		t.Fatal(err)
	}
	var out []engine.Event
	for _, e := range rows {
		if e.Event == event {
			out = append(out, e)
		}
	}
	return out
}

// TestCommandsWaitAWriterOut (CRW-1495 item 6): the commands that need a free run lock (cancel of a run no process holds, cancel of
// an ended run that gives up its executions, answer, resume --detach, the foreground resume with a SCRIPT and the run process) wait a
// writer's hold out, for writerWait at most: a hold of 150 ms is waited out and the command does its work; a hold past the wait is
// refused as a held run, and nothing is written.
func TestCommandsWaitAWriterOut(t *testing.T) {
	type outcome struct {
		code   int
		errOut string
	}
	type subject struct {
		name string
		// setup makes the run of the command and returns its id
		setup func(t *testing.T, pw *processWorld) (string, func() outcome)
		// done checks the command did its work; refused checks it refused and wrote nothing
		done, refused func(t *testing.T, pw *processWorld, run string, o outcome)
	}
	var signalled []int
	// settles counts the settlements the cancel of an ended run asked for
	settles := 0
	subjects := []subject{
		{
			name: "cancel",
			setup: func(t *testing.T, pw *processWorld) (string, func() outcome) {
				pw.newRun("run-dead", "thread-owner").close()
				return "run-dead", func() outcome {
					code, _, errOut := pw.td.do("cancel", "run-dead", "--state", pw.state)
					return outcome{code, errOut}
				}
			},
			done: func(t *testing.T, pw *processWorld, run string, o outcome) {
				ends := runRows(t, pw.world, run, "finished")
				if o.code != 0 || len(ends) != 1 || ends[0].Data["error_kind"] != engine.ErrKindCancelled {
					t.Fatalf("cancel after a writer's hold: exit %d %q, end rows %+v", o.code, o.errOut, ends)
				}
			},
			refused: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if ends := runRows(t, pw.world, run, "finished"); o.code == 0 || !strings.Contains(o.errOut, ErrRunLive.Error()) || len(ends) != 0 {
					t.Fatalf("cancel during a writer's hold past the wait: exit %d %q, end rows %+v", o.code, o.errOut, ends)
				}
			},
		},
		{
			name: "cancel of an ended run",
			setup: func(t *testing.T, pw *processWorld) (string, func() outcome) {
				r := pw.newRun("run-ended", "thread-owner")
				r.finish(engine.RunCompleted, "")
				r.close()
				// the run ended with an execution it still holds: its cancel gives it up under the lock (settleEndedRun)
				pw.td.Settle = func(context.Context, *store.Store, string, []ops.ReleaseRef) (Settlement, error) {
					settles++
					return Settlement{Closed: 1, SlotsReturned: 1}, nil
				}
				return "run-ended", func() outcome {
					code, _, errOut := pw.td.do("cancel", "run-ended", "--state", pw.state)
					return outcome{code, errOut}
				}
			},
			done: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if ends := runRows(t, pw.world, run, "finished"); o.code != 0 || settles != 1 || len(ends) != 1 {
					t.Fatalf("cancel of an ended run after a writer's hold: exit %d %q, %d settlements, end rows %+v", o.code, o.errOut, settles, ends)
				}
			},
			refused: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if o.code == 0 || !strings.Contains(o.errOut, ErrRunLive.Error()) || settles != 0 {
					t.Fatalf("cancel of an ended run during a writer's hold past the wait: exit %d %q, %d settlements", o.code, o.errOut, settles)
				}
			},
		},
		{
			name: "answer",
			setup: func(t *testing.T, pw *processWorld) (string, func() outcome) {
				a := newAsking(t, pw.world)
				id := a.answered(t, "ud-1", nil)
				return "run-q", func() outcome {
					code, reply, errOut := a.answer(pw.td, id, "--no-resume")
					if code == 1 && (reply.Refusal == nil || reply.Refusal.Code != "run_live") {
						t.Errorf("a refused answer: %+v", reply)
					}
					return outcome{code, errOut}
				}
			},
			done: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if rows := runRows(t, pw.world, run, ops.EvDecisionApplied); o.code != 0 || len(rows) != 1 {
					t.Fatalf("answer after a writer's hold: exit %d %q, %d decision_applied rows", o.code, o.errOut, len(rows))
				}
			},
			refused: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if rows := runRows(t, pw.world, run, ops.EvDecisionApplied); o.code != 1 || len(rows) != 0 {
					t.Fatalf("answer during a writer's hold past the wait: exit %d %q, %d decision_applied rows", o.code, o.errOut, len(rows))
				}
			},
		},
		{
			name: "resume --detach",
			setup: func(t *testing.T, pw *processWorld) (string, func() outcome) {
				pw.newRun("run-r", "thread-owner").close()
				pw.spec("run-r", nil)
				return "run-r", func() outcome {
					code, _, errOut := pw.td.do("resume", "run-r", "--state", pw.state, "--detach")
					return outcome{code, errOut}
				}
			},
			done: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if o.code != 0 || len(pw.td.spawned) != 1 {
					t.Fatalf("resume --detach after a writer's hold: exit %d %q, %d spawns", o.code, o.errOut, len(pw.td.spawned))
				}
			},
			refused: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if o.code == 0 || !strings.Contains(o.errOut, ErrRunLive.Error()) || len(pw.td.spawned) != 0 {
					t.Fatalf("resume --detach during a writer's hold past the wait: exit %d %q, %d spawns", o.code, o.errOut, len(pw.td.spawned))
				}
			},
		},
		{
			name: "resume SCRIPT",
			setup: func(t *testing.T, pw *processWorld) (string, func() outcome) {
				scriptResumeEnv(t, pw)
				spec := pw.spec("run-s", nil)
				// the first process is paused by the operator in its first call; the foreground resume then goes on to the end
				sigs := make(chan os.Signal, 2)
				pause := true
				pw.handle = func(r engine.FakeRequest) engine.FakeReply {
					if pause && r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
						sigs <- syscall.SIGUSR1
						time.Sleep(200 * time.Millisecond)
					}
					return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
				}
				if code, errOut := pw.execute(spec, false, sigs); code != 0 {
					t.Fatalf("the first process: exit %d %s", code, errOut)
				}
				pause = false
				return "run-s", func() outcome {
					code, _, errOut := pw.td.do("resume", "run-s", pw.script, "--state", pw.state)
					return outcome{code, errOut}
				}
			},
			done: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if info := pw.info(run); o.code != 0 || info.State != StateCompleted || info.Processes != 2 {
					t.Fatalf("resume SCRIPT after a writer's hold: exit %d %q, %+v", o.code, o.errOut, info)
				}
			},
			refused: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if rows := runRows(t, pw.world, run, "started"); o.code == 0 || !strings.Contains(o.errOut, ErrRunLive.Error()) || len(rows) != 1 {
					t.Fatalf("resume SCRIPT during a writer's hold past the wait: exit %d %q, %d started rows", o.code, o.errOut, len(rows))
				}
			},
		},
		{
			name: "run process",
			setup: func(t *testing.T, pw *processWorld) (string, func() outcome) {
				spec := pw.spec("run-x", nil)
				pw.handle = func(engine.FakeRequest) engine.FakeReply {
					return engine.FakeReply{Tokens: &engine.Tokens{Input: 10, Output: 5, Total: 15}}
				}
				return "run-x", func() outcome {
					code, errOut := pw.execute(spec, false, nil)
					return outcome{code, errOut}
				}
			},
			done: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if info := pw.info(run); o.code != 0 || info.State != StateCompleted {
					t.Fatalf("the run process after a writer's hold: exit %d %q, %+v", o.code, o.errOut, info)
				}
			},
			refused: func(t *testing.T, pw *processWorld, run string, o outcome) {
				if rows := runRows(t, pw.world, run, "started"); o.code != exitLive || len(rows) != 0 {
					t.Fatalf("the run process during a writer's hold past the wait: exit %d %q, %d started rows", o.code, o.errOut, len(rows))
				}
			},
		},
	}
	for _, s := range subjects {
		t.Run(s.name, func(t *testing.T) {
			pw := newProcessWorld(t, twoCalls)
			signalled, settles = nil, 0
			pw.td.Signal = func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil }
			run, do := s.setup(t, pw)
			done := releaseAfter(t, pw.world, run, "audit", 150*time.Millisecond)
			o := do()
			<-done
			s.done(t, pw, run, o)
			if len(signalled) != 0 {
				t.Errorf("signals to %v", signalled)
			}
		})
		t.Run(s.name+"/past the wait", func(t *testing.T) {
			saved := writerWait
			writerWait = 50 * time.Millisecond
			defer func() { writerWait = saved }()
			pw := newProcessWorld(t, twoCalls)
			signalled, settles = nil, 0
			pw.td.Signal = func(pid int, sig syscall.Signal) error { signalled = append(signalled, pid); return nil }
			run, do := s.setup(t, pw)
			done := releaseAfter(t, pw.world, run, "audit", 500*time.Millisecond)
			o := do()
			<-done
			s.refused(t, pw, run, o)
			if len(signalled) != 0 {
				t.Errorf("signals to %v", signalled)
			}
		})
	}
}

// TestTwoCancelsWriteOneEnd (CRW-1495 item 6): two cancels of one run no process holds, the second of which read the run before the
// first stored its row, write one end row: the second reads the end again under its lock and has nothing to cancel.
func TestTwoCancelsWriteOneEnd(t *testing.T) {
	w := newWorld(t)
	w.newRun("run-dead", "thread-owner").close()
	td := w.deps()
	r, err := OpenReader(ctx, w.state, "")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := r.Record(ctx, "run-dead")
	_ = r.Close()
	if err != nil {
		t.Fatal(err)
	}
	cancel := func() (int, string) {
		var out, errOut bytes.Buffer
		c := &command{Deps: td.Deps, ctx: ctx, stdout: &out, stderr: &errOut, name: "cancel"}
		w.close()
		defer w.open()
		return c.cancelDead(w.state, &common{state: w.state}, rec), errOut.String()
	}
	if code, errOut := cancel(); code != 0 {
		t.Fatalf("the first cancel: exit %d %q", code, errOut)
	}
	code, errOut := cancel()
	if ends := runRows(t, w, "run-dead", "finished"); len(ends) != 1 || code != 1 || !strings.Contains(errOut, "nothing to cancel") {
		t.Fatalf("the second cancel: exit %d %q, %d end rows", code, errOut, len(ends))
	}
}
