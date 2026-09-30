package service

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// Timestamp-derived occurrence IDs cannot be normalized: pin the injected domain clocks of a
// relay built for the purpose (the Python console script's were pinned alike until todo 44).
func fixedClockRuntime(t *testing.T) func(string, []string) capture {
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
	return func(home string, args []string) capture {
		prepareParityOwnership(t, home)
		cmd := exec.Command(fixed, append([]string{"relay", "--state", home + "/state"}, args...)...)
		cmd.Env = environment(home)
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
