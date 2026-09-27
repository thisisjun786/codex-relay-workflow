package faults

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestCSeededCLIOracle(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	home, err := os.MkdirTemp("/dev/shm", "fault-c-seed-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "python")
	observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"relationship":"r","turn":"t"},"occurrenceKey":"a","scope":{"projectKey":"CRW"}}`
	code, reply := cliCall(t, goDir, "fault-observe", "--observation", observation)
	if code != 0 {
		t.Fatal(reply)
	}
	id := reply["faultId"].(string)
	publication := publicationID(id, openRecord, triggerOpen)
	if err = os.MkdirAll(pyDir, 0700); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(goDir, "relay.sqlite3"))
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(pyDir, "relay.sqlite3"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"fault-show"}, {"fault-show", "--fault", id}, {"fault-show", "--publication", publication}, {"fault-next"}, {"fault-retry", "--publication", publication}, {"fault-stage", "--fault", id, "--stage", "accepted", "--ref", "r"}, {"fault-queue", "--fault", id, "--kind", "append_comment", "--trigger", "extra"}} {
		t.Run(args[0]+"_"+args[len(args)-1], func(t *testing.T) {
			cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay", "--state", pyDir, "--json"}, args...)...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+filepath.Join(home, "state"), "XDG_CONFIG_HOME="+filepath.Join(home, "config"), "CODEX_HOME="+filepath.Join(home, "codex"), "TMPDIR=/dev/shm")
			want, err := cmd.Output()
			pyCode := 0
			if err != nil {
				if ex, ok := err.(*exec.ExitError); ok {
					pyCode = ex.ExitCode()
				} else {
					t.Fatal(err)
				}
			}
			var got, stderr bytes.Buffer
			goCode, handled := ExecuteAs(context.Background(), "codex-session-relay", append([]string{"--state", goDir, "--json"}, args...), &got, &stderr, nil)
			if !handled {
				t.Fatal("unhandled")
			}
			if pyCode != goCode || !bytes.Equal(want, got.Bytes()) {
				t.Errorf("python (%d): %s\ngo (%d): %s\nstderr: %s", pyCode, want, goCode, got.String(), stderr.String())
			}
		})
	}
}
