package faults

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestF2CLIOracle(t *testing.T) {
	root, e := filepath.Abs("../../..")
	if e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{
		{"fault-fail", "--publication", "missing", "--claim-token", "token", "--error", "failed"},
		{"fault-fail", "--publication", "missing", "--claim-token", "token", "--error", "failed", "--ended"},
		{"fault-adopt", "--fault", "missing", "--external-ref", "CRW-1", "--scope", `{"projectKey":"P"}`},
		{"fault-move", "--fault", "missing", "--scope", `{"projectKey":"P"}`},
		{"fault-update", "--fault", "missing", "--op", "reopen"},
		{"fault-adopt", "--fault", "missing", "--external-ref", "CRW-1", "--scope", `[]`},
		{"fault-adopt", "--fault", "missing", "--external-ref", "CRW-1", "--scope", `{}`},
		{"fault-move", "--fault", "missing", "--scope", `{"workspace":1}`},
		{"fault-move", "--fault", "missing", "--scope", `{"other":[]}`},
		{"fault-update", "--fault", "missing", "--op", "set_project"},
		{"fault-update", "--fault", "missing", "--op", "reopen", "--value", `"extra"`},
		{"fault-update", "--fault", "missing", "--op", "add_label"},
		{"fault-update", "--fault", "missing", "--op", "add_relation", "--value", `{}`},
		{"fault-update", "--fault", "missing", "--op", "invalid"},
		{"fault-move", "--fault", "missing", "--scope", "not-json"},
		{"fault-adopt", "--fault", "missing", "--external-ref", "CRW-1", "--scope", `{"projectKey":"P","workspace":false}`},
		{"fault-update", "--fault", "missing", "--op", "add_label", "--value", "not-json"},
	} {
		t.Run(args[0], func(t *testing.T) {
			home, e := os.MkdirTemp("/dev/shm", "f2-oracle-")
			if e != nil {
				t.Fatal(e)
			}
			defer os.RemoveAll(home)
			py := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay", "--state", filepath.Join(home, "py"), "--json"}, args...)...)
			py.Dir = root
			py.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
			want, err := py.Output()
			pyStderr := []byte(nil)
			pyCode := 0
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					pyCode = exit.ExitCode()
					pyStderr = exit.Stderr
				} else {
					t.Fatal(err)
				}
			}
			var got, stderr bytes.Buffer
			code, handled := ExecuteAs(context.Background(), "codex-session-relay", append([]string{"--state", filepath.Join(home, "go"), "--json"}, args...), &got, &stderr, nil)
			if !handled || code != pyCode || !bytes.Equal(got.Bytes(), want) || !bytes.Equal(stderr.Bytes(), pyStderr) {
				t.Errorf("python (%d) %s; go (%d) %s; stderr %s", pyCode, want, code, got.String(), stderr.String())
			}
		})
	}
}
