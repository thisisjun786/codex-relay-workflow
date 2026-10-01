package service

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Timestamp-derived occurrence IDs cannot be normalized: pin the injected domain clocks of a
// relay built for the purpose (the Python console script's were pinned alike until todo 44).
func fixedClockRuntime(t *testing.T) func(string, []string) capture {
	t.Helper()
	// os.Executable must stay in the installation directory, so the clock-injected build is
	// copied there, beside the shared one.
	fixed := filepath.Join(filepath.Dir(testBinary), "fixed-crw")
	if _, err := os.Stat(fixed); errors.Is(err, os.ErrNotExist) {
		if err = testsupport.CopyBinary(testsupport.BuildCRW(t, "-ldflags", "-X github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter.testClock=1700000000"), fixed); err != nil {
			t.Fatal(err)
		}
	} else if err != nil {
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
