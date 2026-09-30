package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fresh disposable state per process; compare the actual CLI JSON bytes, not selected fields, and
// whether the command left its state directory in place.
func TestCCLIOracle(t *testing.T) {
	goldenParent(t)
	cases := [][]string{
		{"fault-show"}, {"fault-next"}, {"fault-show", "--fault", "missing"},
		{"fault-show", "--fault", "missing", "--product", "crw"},
		{"fault-show", "--publication", "missing"},
		{"fault-next", "--limit", "0"}, {"fault-show", "--limit", "0"},
		{"fault-retry", "--publication", "missing"}, {"fault-cancel", "--publication", "missing", "--reason", "r"},
		{"fault-stage", "--fault", "missing", "--stage", "accepted", "--ref", "r"},
		{"fault-queue", "--fault", "missing", "--kind", "project_create", "--trigger", "t"},
	}
	for _, args := range cases {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			home, e := os.MkdirTemp("/dev/shm", "fault-c-oracle-")
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			// The directory keeps the name the goldens were first taken with.
			state := filepath.Join(home, "python")
			var got, stderr bytes.Buffer
			code, handled := executeAsCLI(context.Background(), append([]string{"--state", state, "--json"}, args...), &got, &stderr)
			if !handled {
				t.Fatal("unhandled")
			}
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: code, Stdout: got.String(), Created: created(t, state)})
		})
	}
}
