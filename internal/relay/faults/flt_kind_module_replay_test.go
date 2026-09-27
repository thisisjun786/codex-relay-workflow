package faults

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// FLT-33: each static-registry replacement path compares the complete CLI
// output and exit status with Python's live --kind-module behavior.
func Test22_FLT_33_KindModuleWholeOutput(t *testing.T) {
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"json", "os.path", "codex_session_relay.projects", "no_such_module_crw205"} {
		t.Run(module, func(t *testing.T) {
			home, err := os.MkdirTemp("/dev/shm", "flt33-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			args := []string{"--kind-module", module, "--state", filepath.Join(home, "py"), "--json", "fault-target", "--product", "crw", "--team", "team-relay"}
			cmd := exec.Command("uv", append([]string{"run", "--no-sync", "codex-session-relay"}, args...)...)
			cmd.Dir = root
			cmd.Env = append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/state", "XDG_CONFIG_HOME="+home+"/config", "CODEX_HOME="+home+"/codex", "TMPDIR=/dev/shm")
			wantOut, pyErr := cmd.Output()
			wantErr := []byte(nil)
			wantCode := 0
			if pyErr != nil {
				exit, ok := pyErr.(*exec.ExitError)
				if !ok {
					t.Fatal(pyErr)
				}
				wantCode, wantErr = exit.ExitCode(), exit.Stderr
			}
			args[3] = filepath.Join(home, "go")
			var gotOut, gotErr bytes.Buffer
			gotCode, handled := ExecuteAs(context.Background(), "codex-session-relay", args, &gotOut, &gotErr, nil)
			if !handled || gotCode != wantCode || !bytes.Equal(gotOut.Bytes(), wantOut) || !bytes.Equal(gotErr.Bytes(), wantErr) {
				t.Fatalf("Python %d stdout=%q stderr=%q; Go %d stdout=%q stderr=%q", wantCode, wantOut, wantErr, gotCode, gotOut.Bytes(), gotErr.Bytes())
			}
		})
	}
}
