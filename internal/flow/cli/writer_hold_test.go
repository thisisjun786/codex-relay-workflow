package cli

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
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
