package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// FLT-33: each static-registry replacement path compares the complete CLI
// output and exit status with Python's live --kind-module behavior.
func Test22_FLT_33_KindModuleWholeOutput(t *testing.T) {
	for _, module := range []string{"json", "os.path", "codex_session_relay.projects", "no_such_module_crw205"} {
		t.Run(module, func(t *testing.T) {
			home, err := os.MkdirTemp("/dev/shm", "flt33-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(home) })
			args := []string{"--kind-module", module, "--state", filepath.Join(home, "py"), "--json", "fault-target", "--product", "crw", "--team", "team-relay"}
			answer := pyCLIRun(t, home, "", args, true, pyHomeEnv(home)...)
			wantOut, wantErr, wantCode := []byte(answer.Stdout), []byte(answer.Stderr), answer.Code
			args[3] = filepath.Join(home, "go")
			var gotOut, gotErr bytes.Buffer
			gotCode, handled := executeAsCLI(context.Background(), args, &gotOut, &gotErr)
			if !handled || gotCode != wantCode || !bytes.Equal(gotOut.Bytes(), wantOut) || !bytes.Equal(gotErr.Bytes(), wantErr) {
				t.Fatalf("Python %d stdout=%q stderr=%q; Go %d stdout=%q stderr=%q", wantCode, wantOut, wantErr, gotCode, gotOut.Bytes(), gotErr.Bytes())
			}
		})
	}
}
