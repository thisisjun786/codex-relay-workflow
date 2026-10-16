package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// CRW-1570 D2: every detached launch of a run's process (start, `crw flow answer`, `crw flow resume RUN --detach` and the automatic
// resume of `crw flow next`) goes through DetachedCommand, which hands the child the environment the run's spec records; a launch
// whose recorded TMPDIR is gone is refused before anything is started.

// testEnvProbe names, in the environment of this test binary, the file the binary started in its probe mode writes its environment to
// (TestMain): a relaunched run process that is this binary shows the environment it was started with.
const testEnvProbe = "CRW_TEST_ENV_PROBE"

// envProbeMain is the probe mode of the test binary: it writes its environment to the file testEnvProbe names (through a temporary
// name and a rename, so the file is whole once it exists) and lives until it is killed, 30 seconds at most, so that the test reads
// /proc/<pid>/environ of a live process.
func envProbeMain(file string) int {
	part := file + ".part"
	if err := os.WriteFile(part, []byte(strings.Join(os.Environ(), "\x00")), 0o600); err != nil {
		return 1
	}
	if err := os.Rename(part, file); err != nil {
		return 1
	}
	time.Sleep(30 * time.Second)
	return 0
}

// probeLaunches makes the run processes the test starts this test binary in its probe mode, writing their environment to file.
func probeLaunches(t *testing.T, file string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return self, nil }
	t.Cleanup(func() { executable = old })
	t.Setenv(testEnvProbe, file)
}

// childEnv is the environment of the probe child pid: /proc/<pid>/environ while it lives (the one it was started with), or the file
// it wrote when /proc cannot be read. The child is killed and reaped.
func childEnv(t *testing.T, pid int, file string) []string {
	t.Helper()
	defer func() {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		_, _ = syscall.Wait4(pid, &ws, 0, nil)
	}()
	deadline := time.Now().Add(20 * time.Second)
	for {
		if _, err := os.Stat(file); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process %d wrote no environment to %s", pid, file)
		}
		time.Sleep(20 * time.Millisecond)
	}
	raw, err := os.ReadFile("/proc/" + itoa(pid) + "/environ")
	if err != nil {
		if raw, err = os.ReadFile(file); err != nil {
			t.Fatal(err)
		}
	}
	return strings.Split(strings.TrimRight(string(raw), "\x00"), "\x00")
}

// tmpdirs are the TMPDIR entries of an environment.
func tmpdirs(env []string) []string {
	var out []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "TMPDIR=") {
			out = append(out, strings.TrimPrefix(kv, "TMPDIR="))
		}
	}
	return out
}

// wantOnlyTMPDIR fails unless env holds exactly one TMPDIR, want.
func wantOnlyTMPDIR(t *testing.T, what string, env []string, want string) {
	t.Helper()
	if got := tmpdirs(env); len(got) != 1 || got[0] != want {
		t.Errorf("%s: TMPDIR entries %q, want exactly [%s]", what, got, want)
	}
}

// callerTMPDIR sets the TMPDIR of the test process (the shell that relaunches the run) to dir, or unsets it for "".
func callerTMPDIR(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("TMPDIR", dir)
	if dir == "" {
		_ = os.Unsetenv("TMPDIR")
	}
}

func TestDetachedCommandTakesTheRunsTMPDIR(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	log, err := os.Create(filepath.Join(t.TempDir(), "process.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	req := SpawnRequest{Spec: Spec{Schema: SpecSchema, RunID: "run-env", Env: map[string]string{"TMPDIR": a}}, State: t.TempDir(), Resume: true}
	for _, caller := range []string{b, ""} {
		callerTMPDIR(t, caller)
		cmd := DetachedCommand("/bin/crw", req, log)
		wantOnlyTMPDIR(t, "the command of a run that records TMPDIR, caller TMPDIR "+cell(caller), cmd.Env, a)
		if len(cmd.Env) == 0 || !contains(cmd.Env, "PATH="+os.Getenv("PATH")) {
			t.Errorf("the command does not keep the caller's other variables: %q", cmd.Env)
		}
	}
	old := req
	old.Spec.Env = nil
	if cmd := DetachedCommand("/bin/crw", old, log); cmd.Env != nil {
		t.Errorf("the command of a run started before the record sets an environment: %q", cmd.Env)
	}
}

func TestRunEnvironReplacesEachRecordedKeyOnce(t *testing.T) {
	got := runEnviron([]string{"A=1", "TMPDIR=/b", "B=2", "TMPDIR=/c", "Z=9"}, map[string]string{"TMPDIR": "/a", "Z": "0"})
	want := []string{"A=1", "B=2", "TMPDIR=/a", "Z=0"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("runEnviron = %q, want %q", got, want)
	}
	if got := runEnviron([]string{"A=1"}, nil); strings.Join(got, " ") != "A=1" {
		t.Errorf("runEnviron without a record = %q", got)
	}
}

func TestRelaunchedRunProcessHasTheRunsTMPDIR(t *testing.T) {
	a := t.TempDir()
	for _, caller := range []string{t.TempDir(), ""} {
		t.Run("caller TMPDIR "+cell(caller), func(t *testing.T) {
			state := t.TempDir()
			file := filepath.Join(t.TempDir(), "environ")
			probeLaunches(t, file)
			callerTMPDIR(t, caller)
			pid, err := SpawnDetached(ctx, SpawnRequest{Spec: Spec{Schema: SpecSchema, RunID: "run-env", Env: map[string]string{"TMPDIR": a}}, State: state, Resume: true, Dir: state})
			if err != nil {
				t.Fatal(err)
			}
			wantOnlyTMPDIR(t, "the relaunched run process", childEnv(t, pid, file), a)
		})
	}
}

// envAsking is the asking run of answer_test.go with a spec that records TMPDIR=a (nil a: a spec written before the record).
func envAsking(t *testing.T, w *world, env map[string]string) *asking {
	t.Helper()
	a := newAsking(t, w)
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: "run-q", Workflow: "project-lane", Thread: "thread-owner", CreatedAt: "2026-10-12T08:00:00Z", Env: env}); err != nil {
		t.Fatal(err)
	}
	return a
}

// deadEnvSessionRun writes a session run whose process died (its started row and no end, no lock held) with a spec that records env.
func deadEnvSessionRun(t *testing.T, w *world, run string, env map[string]string) {
	t.Helper()
	if err := WriteSpec(w.state, Spec{Schema: SpecSchema, RunID: run, Workflow: "hello", Script: helloScript(t), Thread: sessOwner, Backend: BackendSession, CreatedAt: "2026-10-14T00:00:00Z", Env: env}); err != nil {
		t.Fatal(err)
	}
	r := w.newRun(run, sessOwner)
	r.close()
}

// relaunchers are the three commands that relaunch a run's process from a shell, each run against a world of its own; they return
// the pid of the process they started (0: none) and their output.
var relaunchers = []struct {
	name string
	run  func(t *testing.T, env map[string]string) (pid int, out string)
}{
	{"answer", func(t *testing.T, env map[string]string) (int, string) {
		w := newWorld(t)
		a := envAsking(t, w, env)
		td := w.deps()
		td.Getenv = envOf(nil)
		td.Spawn = SpawnDetached
		id := a.answered(t, "ud-env", nil)
		code, reply, errOut := a.answer(td, id)
		if code != 0 || !reply.Applied || reply.Resumed == 0 {
			t.Fatalf("the answer: exit %d %+v %s", code, reply, errOut)
		}
		return reply.Resumed, errOut
	}},
	{"resume --detach", func(t *testing.T, env map[string]string) (int, string) {
		w := newWorld(t)
		envAsking(t, w, env)
		td := w.deps()
		td.Getenv = envOf(nil)
		td.Spawn = SpawnDetached
		code, out, errOut := td.do("resume", "run-q", "--detach", "--state", w.state)
		var pid int
		if code != 0 || !strings.Contains(out, "resumed (process ") {
			t.Fatalf("resume --detach: exit %d %q %q", code, out, errOut)
		}
		raw := strings.TrimSuffix(strings.TrimSpace(out[strings.Index(out, "(process ")+len("(process "):]), ")")
		for _, c := range raw {
			pid = pid*10 + int(c-'0')
		}
		return pid, errOut
	}},
	{"next", func(t *testing.T, env map[string]string) (int, string) {
		t.Setenv("HOME", t.TempDir())
		w := newWorld(t)
		run := "run-0123456789e1"
		deadEnvSessionRun(t, w, run, env)
		td := w.deps()
		td.Getenv = envOf(nil)
		td.Spawn = SpawnDetached
		ans := nextOf(t, td, run)
		if ans.Resumed == 0 {
			t.Fatalf("next did not resume the dead run: %+v", ans)
		}
		return ans.Resumed, strings.Join(ans.Warnings, "\n")
	}},
}

func TestAnswerResumeNextRelaunchWithTheRunsTMPDIR(t *testing.T) {
	isolateGit(t)
	a := t.TempDir()
	for _, r := range relaunchers {
		t.Run(r.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "environ")
			probeLaunches(t, file)
			callerTMPDIR(t, t.TempDir())
			pid, _ := r.run(t, map[string]string{"TMPDIR": a})
			wantOnlyTMPDIR(t, r.name+"'s run process", childEnv(t, pid, file), a)
		})
	}
}

func TestOldRunRelaunchInherits(t *testing.T) {
	isolateGit(t)
	for _, r := range relaunchers {
		t.Run(r.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "environ")
			probeLaunches(t, file)
			b := t.TempDir()
			callerTMPDIR(t, b)
			pid, _ := r.run(t, nil)
			wantOnlyTMPDIR(t, r.name+"'s run process of a spec without env", childEnv(t, pid, file), b)
		})
	}
}

func TestResumeDetachAndNextRefuseARemovedRecordedTMPDIR(t *testing.T) {
	isolateGit(t)
	t.Run("resume --detach", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "gone")
		w := newWorld(t)
		envAsking(t, w, map[string]string{"TMPDIR": a})
		td := w.deps()
		before := len(mustLoad(t, w))
		code, out, errOut := td.do("resume", "run-q", "--detach", "--state", w.state)
		if code != 1 || !strings.Contains(errOut, "recorded TMPDIR "+a) || strings.Contains(out, "resumed") || len(td.spawned) != 0 {
			t.Errorf("resume --detach with the recorded TMPDIR gone: exit %d %q %q, %d spawned", code, out, errOut, len(td.spawned))
		}
		if after := len(mustLoad(t, w)); after != before {
			t.Errorf("resume --detach appended %d rows", after-before)
		}
	})
	t.Run("next", func(t *testing.T) {
		t.Setenv("HOME", t.TempDir())
		a := filepath.Join(t.TempDir(), "gone")
		w := newWorld(t)
		run := "run-0123456789e2"
		deadEnvSessionRun(t, w, run, map[string]string{"TMPDIR": a})
		td := w.deps()
		before, _, err := loadRun(w, run)
		if err != nil {
			t.Fatal(err)
		}
		ans := nextOf(t, td, run)
		warned := false
		for _, warn := range ans.Warnings {
			if strings.Contains(warn, "its resume did not start") && strings.Contains(warn, "recorded TMPDIR "+a) {
				warned = true
			}
		}
		if ans.Resumed != 0 || len(td.spawned) != 0 || !warned {
			t.Errorf("next with the recorded TMPDIR gone: %+v, %d spawned", ans, len(td.spawned))
		}
		if after, _, _ := loadRun(w, run); len(after) != len(before) {
			t.Errorf("next appended %d rows", len(after)-len(before))
		}
	})
	t.Run("SpawnDetached", func(t *testing.T) {
		a := filepath.Join(t.TempDir(), "gone")
		state := t.TempDir()
		// a process it starts anyway is this binary in the probe mode, killed below, never the test binary running its tests
		file := filepath.Join(t.TempDir(), "environ")
		probeLaunches(t, file)
		pid, err := SpawnDetached(ctx, SpawnRequest{Spec: Spec{Schema: SpecSchema, RunID: "run-env", Env: map[string]string{"TMPDIR": a}}, State: state, Resume: true, Dir: state})
		if pid > 0 {
			childEnv(t, pid, file)
		}
		if err == nil || pid != 0 || !strings.Contains(err.Error(), "recorded TMPDIR "+a) {
			t.Errorf("SpawnDetached with the recorded TMPDIR gone: %d %v", pid, err)
		}
		if _, err := os.Stat(filepath.Join(RunDir(state, "run-env"), "process.log")); err == nil {
			t.Errorf("SpawnDetached opened the process log of a run it refuses")
		}
	})
}
