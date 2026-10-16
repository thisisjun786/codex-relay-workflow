package cli

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1570 D3: a run that works in this process (the run process `crw flow _run`, the foreground `crw flow resume RUN` that runs it
// through execute, and the engine's `crw flow resume RUN SCRIPT`) puts the environment its spec records into the environment of the
// process before anything of the run is done, and puts back what was there after; a recorded TMPDIR that cannot be used refuses the
// run before it takes a lock.

// pausedEnvRun runs the first process of a run of pw's script with the spec mutate gives and pauses it in its first call, so that a
// resume has a run to take. The ApplyEnv calls of that first process are forgotten.
func pausedEnvRun(t *testing.T, pw *processWorld, run string, mutate func(*Spec)) Spec {
	t.Helper()
	spec := pw.spec(run, mutate)
	sigs := make(chan os.Signal, 2)
	pause := true
	pw.handle = func(r engine.FakeRequest) engine.FakeReply {
		if pause && r.Kind == "turn" && strings.Contains(r.Prompt, "first") {
			sigs <- syscall.SIGUSR1
			time.Sleep(200 * time.Millisecond)
		}
		return engine.FakeReply{}
	}
	if code, errOut := pw.execute(spec, false, sigs); code != 0 {
		t.Fatalf("the first process: exit %d %s", code, errOut)
	}
	if info := pw.info(run); info.State != StatePaused {
		t.Fatalf("the first process did not pause the run: %+v", info)
	}
	pause = false
	pw.td.envs.reset()
	return spec
}

// TestForegroundResumeAppliesTheRunsEnv: `crw flow resume RUN` and `crw flow resume RUN SCRIPT` apply the spec's recorded env once,
// before the engine sends the run's first turn, and restore it once after the run is done; a spec written before the record (no env)
// applies nothing.
func TestForegroundResumeAppliesTheRunsEnv(t *testing.T) {
	for _, path := range []string{"spec", "script"} {
		for _, recorded := range []bool{true, false} {
			name := path + "/without env"
			if recorded {
				name = path + "/with env"
			}
			t.Run(name, func(t *testing.T) {
				pw := newProcessWorld(t, twoCalls)
				if path == "script" {
					scriptResumeEnv(t, pw)
				}
				a := t.TempDir()
				run := "run-apply-" + path
				pausedEnvRun(t, pw, run, func(s *Spec) {
					if recorded {
						s.Env = map[string]string{"TMPDIR": a}
					}
				})
				var duringApplied, duringRestored []int
				pw.handle = func(r engine.FakeRequest) engine.FakeReply {
					if r.Kind == "turn" {
						applied, restored := pw.td.envs.counts()
						duringApplied, duringRestored = append(duringApplied, len(applied)), append(duringRestored, restored)
					}
					return engine.FakeReply{}
				}
				args := []string{"resume", run}
				if path == "script" {
					args = append(args, pw.script)
				}
				code, out, errOut := pw.td.do(append(args, "--state", pw.state)...)
				if code != 0 {
					t.Fatalf("the resume: exit %d\n%s\n%s", code, out, errOut)
				}
				if len(duringApplied) == 0 {
					t.Fatal("the resume sent no turn")
				}
				applied, restored := pw.td.envs.counts()
				if !recorded {
					if len(applied) != 0 || restored != 0 {
						t.Errorf("a spec without env applied %v (restored %d)", applied, restored)
					}
					return
				}
				if len(applied) != 1 || len(applied[0]) != 1 || applied[0]["TMPDIR"] != a || restored != 1 {
					t.Errorf("the resume applied %v and restored %d times, want {TMPDIR: %s} once, restored once", applied, restored, a)
				}
				for i := range duringApplied {
					if duringApplied[i] != 1 || duringRestored[i] != 0 {
						t.Errorf("turn %d was sent with %d applied and %d restored, want the env applied and not yet restored", i, duringApplied[i], duringRestored[i])
					}
				}
			})
		}
	}
}

// TestExecuteRefusesARemovedRecordedTMPDIR: a run whose recorded TMPDIR is gone is refused by the process that would run it (exit 2,
// the reason naming the directory) before it takes the run lock or a process lock, before the world opens and before anything is
// applied; the foreground `crw flow resume RUN SCRIPT` refuses it the same way.
func TestExecuteRefusesARemovedRecordedTMPDIR(t *testing.T) {
	t.Run("execute", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		a := filepath.Join(t.TempDir(), "gone")
		spec := pw.spec("run-gone", func(s *Spec) { s.Env = map[string]string{"TMPDIR": a} })
		opened := 0
		world := pw.td.World
		pw.td.World = func(c context.Context, req WorldRequest) (World, error) { opened++; return world(c, req) }
		code, errOut := pw.execute(spec, false, nil)
		if code != exitUnusable || !strings.Contains(errOut, a) || !strings.Contains(errOut, "does not exist") {
			t.Errorf("the process of a run whose recorded TMPDIR is gone: exit %d %q, want %d naming %s", code, errOut, exitUnusable, a)
		}
		noRunTrace(t, pw, "run-gone", opened)
	})
	t.Run("resume script", func(t *testing.T) {
		pw := newProcessWorld(t, twoCalls)
		scriptResumeEnv(t, pw)
		a := t.TempDir()
		run := "run-gone-script"
		pausedEnvRun(t, pw, run, func(s *Spec) { s.Env = map[string]string{"TMPDIR": a} })
		if err := os.Remove(a); err != nil {
			t.Fatal(err)
		}
		before, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		locksBefore := runLockFiles(t, pw.state, run)
		sent := 0
		pw.handle = func(r engine.FakeRequest) engine.FakeReply { sent++; return engine.FakeReply{} }
		code, out, errOut := pw.td.do("resume", run, pw.script, "--state", pw.state)
		if code != exitUnusable || !strings.Contains(errOut, a) || !strings.Contains(errOut, "does not exist") {
			t.Errorf("resume RUN SCRIPT of a run whose recorded TMPDIR is gone: exit %d, out %q, err %q", code, out, errOut)
		}
		after, _, err := loadRun(pw.world, run)
		if err != nil {
			t.Fatal(err)
		}
		if len(after) != len(before) || sent != 0 {
			t.Errorf("the refused resume appended %d rows and sent %d requests", len(after)-len(before), sent)
		}
		if applied, _ := pw.td.envs.counts(); len(applied) != 0 {
			t.Errorf("the refused resume applied %v", applied)
		}
		if locks := runLockFiles(t, pw.state, run); LockHeld(pw.state, run) || !slices.Equal(locks, locksBefore) {
			t.Errorf("the refused resume took a lock: run lock held %v, lock files %v, before %v", LockHeld(pw.state, run), locks, locksBefore)
		}
	})
}

// noRunTrace fails when the refused process of run left anything: a journal row, a run lock or process lock file, an opened world or
// an applied env.
func noRunTrace(t *testing.T, pw *processWorld, run string, opened int) {
	t.Helper()
	if events, _, err := loadRun(pw.world, run); err != nil || len(events) != 0 {
		t.Errorf("the refused process wrote %d rows (%v)", len(events), err)
	}
	if opened != 0 {
		t.Errorf("the refused process opened its world %d times", opened)
	}
	if applied, _ := pw.td.envs.counts(); len(applied) != 0 {
		t.Errorf("the refused process applied %v", applied)
	}
	if LockHeld(pw.state, run) {
		t.Error("the refused process holds the run lock")
	}
	if files := runLockFiles(t, pw.state, run); len(files) != 0 {
		t.Errorf("the refused process left lock files %v", files)
	}
}

// runLockFiles are the lock files in the run's directory: the run lock and the process locks.
func runLockFiles(t *testing.T, state, run string) []string {
	t.Helper()
	entries, err := os.ReadDir(RunDir(state, run))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var locks []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "lock") {
			locks = append(locks, e.Name())
		}
	}
	return locks
}

// TestDefaultApplyEnvRestores (not parallel: it sets the process environment): the binary's ApplyEnv sets each recorded variable in
// the process environment, and its restore puts back the value there was, or unsets one there was not; an empty record changes
// nothing. A Deps built without ApplyEnv applies nothing.
func TestDefaultApplyEnvRestores(t *testing.T) {
	if defaultDeps().ApplyEnv == nil {
		t.Fatal("the binary's Deps apply no recorded env")
	}
	a, b := t.TempDir(), t.TempDir()
	for _, set := range []bool{true, false} {
		t.Setenv("TMPDIR", b)
		if !set {
			if err := os.Unsetenv("TMPDIR"); err != nil {
				t.Fatal(err)
			}
		}
		restore, err := defaultDeps().ApplyEnv(map[string]string{"TMPDIR": a})
		if err != nil {
			t.Fatal(err)
		}
		if v, ok := os.LookupEnv("TMPDIR"); !ok || v != a || os.TempDir() != a {
			t.Errorf("TMPDIR set %v: applied it is %q (%v), os.TempDir %q, want %s", set, v, ok, os.TempDir(), a)
		}
		restore()
		v, ok := os.LookupEnv("TMPDIR")
		if set && (!ok || v != b) {
			t.Errorf("a TMPDIR that was %s is %q (%v) after the restore", b, v, ok)
		}
		if !set && ok {
			t.Errorf("a TMPDIR that was unset is %q after the restore", v)
		}
	}
	t.Setenv("TMPDIR", b)
	for _, rec := range []map[string]string{nil, {}} {
		restore, err := defaultDeps().ApplyEnv(rec)
		if err != nil {
			t.Fatal(err)
		}
		if v := os.Getenv("TMPDIR"); v != b {
			t.Errorf("an empty record %v changed TMPDIR to %q", rec, v)
		}
		restore()
		if v := os.Getenv("TMPDIR"); v != b {
			t.Errorf("the restore of an empty record changed TMPDIR to %q", v)
		}
	}
	// a hand-built Deps without ApplyEnv runs a run that records env, applying nothing
	pw := newProcessWorld(t, twoCalls)
	pw.td.ApplyEnv = nil
	spec := pw.spec("run-nil-apply", func(s *Spec) { s.Env = map[string]string{"TMPDIR": a} })
	if code, errOut := pw.execute(spec, false, nil); code != 0 {
		t.Fatalf("a Deps without ApplyEnv: exit %d %s", code, errOut)
	}
	if v := os.Getenv("TMPDIR"); v != b {
		t.Errorf("a Deps without ApplyEnv set TMPDIR to %q", v)
	}
}
