package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Timestamp-derived occurrence IDs cannot be normalized: pin the injected domain
// clocks on both sides, while still executing the unchanged Python console script.
func fixedClockRuntime(t *testing.T) func(string, bool, []string) capture {
	t.Helper()
	bin := filepath.Join(filepath.Dir(testBinary), "clock", "codex-session-relay")
	if err := os.MkdirAll(filepath.Dir(bin), 0700); err != nil {
		t.Fatal(err)
	}
	// os.Executable must stay in the same installation directory, including this
	// deterministic build; replacing the file does not affect existing processes.
	cmd := exec.Command("go", "build", "-ldflags", "-X github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter.testClock=1700000000", "-o", bin, "./cmd/crw")
	cmd.Dir = testRoot
	// TestMain isolated HOME after building; reuse the caller's populated Go cache.
	cmd.Env = buildEnvironment
	if raw, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixed clock binary: %v %s", err, raw)
	}
	fixed := filepath.Join(filepath.Dir(testBinary), "fixed-crw")
	raw, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(fixed, raw, 0700); err != nil {
		t.Fatal(err)
	}
	hooks := t.TempDir()
	if err = os.WriteFile(filepath.Join(hooks, "sitecustomize.py"), []byte("from codex_session_relay import clock\nclock.SystemClock = lambda: clock.FakeClock(1700000000)\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return func(home string, python bool, args []string) capture {
		prepareParityOwnership(t, home, python, args)
		program := fixed
		argv := append([]string{"relay", "--state", home + "/state"}, args...)
		env := environment(home)
		if python {
			program = testPython
			argv = append([]string{"--state", home + "/state"}, args...)
			env = environmentSet(env, "PYTHONPATH", hooks+string(os.PathListSeparator)+os.Getenv("PYTHONPATH"))
		}
		cmd := exec.Command(program, argv...)
		cmd.Env = env
		var out, stderr bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &stderr
		err := cmd.Run()
		if err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatal(err)
			}
		}
		return capture{out.String(), stderr.String(), cmd.ProcessState.ExitCode()}
	}
}
