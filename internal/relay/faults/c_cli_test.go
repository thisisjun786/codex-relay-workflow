package faults

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Fresh disposable state per process; compare the actual CLI JSON bytes, not selected fields.
func TestCCLIOracle(t *testing.T) {
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
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
			py := filepath.Join(home, "python")
			goDir := filepath.Join(home, "go")
			cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay", "--state", py, "--json"}, args...)...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
			want, e := cmd.Output()
			pyCode := 0
			if e != nil {
				if exit, ok := e.(*exec.ExitError); ok {
					pyCode = exit.ExitCode()
				} else {
					t.Fatal(e)
				}
			}
			goDir = oracleState(t, py, goDir)
			var got, stderr bytes.Buffer
			code, handled := executeAsCLI(context.Background(), append([]string{"--state", goDir, "--json"}, args...), &got, &stderr)
			if !handled {
				t.Fatal("unhandled")
			}
			neverCreated(t, py, goDir)
			if pyCode != code || !bytes.Equal(want, got.Bytes()) {
				t.Errorf("python (%d): %s\ngo (%d): %s\nstderr: %s", pyCode, want, code, got.String(), stderr.String())
			}
		})
	}
}
