package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FLT-33: each static-registry replacement path compares the complete CLI
// output and exit status with the golden, which began as Python's --kind-module behavior.
func Test22_FLT_33_KindModuleWholeOutput(t *testing.T) {
	goldenParent(t)
	for _, module := range []string{"json", "os.path", "codex_session_relay.projects", "no_such_module_crw205"} {
		t.Run(module, func(t *testing.T) {
			home, err := os.MkdirTemp("/dev/shm", "flt33-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			args := []string{"--kind-module", module, "--state", filepath.Join(home, "go"), "--json", "fault-target", "--product", "crw", "--team", "team-relay"}
			var gotOut, gotErr bytes.Buffer
			gotCode := executeAsCLI(context.Background(), args, &gotOut, &gotErr)
			question := append([]string{"--kind-module", module}, args[4:]...)
			checkGolden(t, "relay "+strings.Join(question, " "), question, runPathsOf(t, home), cliGolden{Code: gotCode, Stdout: gotOut.String(), Stderr: gotErr.String()})
		})
	}
}
