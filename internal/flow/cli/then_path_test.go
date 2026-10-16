package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// TestThenRunsWithoutCrwOnPath (CRW-1572 repro): the then of a spawn step that `crw flow next` printed runs as printed in a shell
// whose PATH has no crw. The crw under test is a copy at a path of its own, started with no CRW_BIN; B0's then starts with the bare
// crw, which the shell does not find (exit 127).
func TestThenRunsWithoutCrwOnPath(t *testing.T) {
	if testing.Short() {
		t.Skip("runs the built crw")
	}
	crw := testsupport.CRWAt(t, filepath.Join(t.TempDir(), "x y", "bin", "crw"))
	w := newWorld(t)
	run := "run-0123456789c1"
	sessionSpec(t, w, run)
	lock, err := TakeLock(w.state, run, "_run", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	released := false
	defer func() {
		if !released {
			lock.Release()
		}
	}()
	bus := engine.NewFileStepBus(RunDir(w.state, run))
	step := engine.Step{Run: run, Call: "c_000000000001", K: 1, Attempt: 1, Op: "c_000000000001/t1-a1", Role: "explorer", Action: engine.StepSpawn,
		AgentType: "explorer", Model: "gpt-fake-sol", ReasoningEffort: "high", Message: "crw-flow-op: c_000000000001/t1-a1\nCRW-ROLE: explorer\n\nlook"}
	if err := bus.Publish(step); err != nil {
		t.Fatal(err)
	}
	// the owner's environment: the run's state is the one its commands select (the then names no --state), no CRW_BIN
	var env []string
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, host.BinEnv+"=") && !strings.HasPrefix(kv, "CODEX_SESSION_RELAY_STATE=") {
			env = append(env, kv)
		}
	}
	env = append(env, "CODEX_SESSION_RELAY_STATE="+w.state)
	cmd := exec.Command(crw, "flow", "next", run, "--json")
	cmd.Env = env
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		t.Fatalf("crw flow next: %v\n%s\n%s", err, out.String(), errOut.String())
	}
	var ans NextAnswer
	if err := json.Unmarshal(out.Bytes(), &ans); err != nil {
		t.Fatalf("%v\n%s", err, out.String())
	}
	var then string
	for _, st := range ans.Steps {
		if st.Action == engine.StepSpawn {
			then = st.Then
		}
	}
	if then == "" {
		t.Fatalf("no spawn step with a then: %+v", ans)
	}
	then = strings.ReplaceAll(then, "<the id spawn_agent returned>", "agent-1")
	// no process holds the run when the owner reports: dispatched queues the report and answers at once
	lock.Release()
	released = true
	shellEnv := []string{"PATH=" + t.TempDir()}
	for _, kv := range env {
		if !strings.HasPrefix(kv, "PATH=") {
			shellEnv = append(shellEnv, kv)
		}
	}
	sh := exec.Command("/bin/sh", "-c", then)
	sh.Env, sh.Dir = shellEnv, t.TempDir()
	got, err := sh.CombinedOutput()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	if code == 127 || strings.Contains(string(got), "not found") {
		t.Fatalf("the then %q does not run without crw on PATH: exit %d\n%s", then, code, got)
	}
	if code != 0 || !strings.Contains(string(got), "queued") {
		t.Errorf("the then %q ran but did not queue the report: exit %d\n%s", then, code, got)
	}
	if reps, _ := bus.Reports(); len(reps) != 1 || reps[0].Agent != "agent-1" || reps[0].Call != step.Call {
		t.Errorf("the inbox after the then: %+v", reps)
	}
}
