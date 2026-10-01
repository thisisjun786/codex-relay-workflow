package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestF2CLIOracle(t *testing.T) {
	goldenParent(t)
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
			answer := pyCLIRun(t, home, "", append([]string{"--state", filepath.Join(home, "py"), "--json"}, args...), true, pyHomeEnv(home)...)
			want, pyStderr, pyCode := []byte(answer.Stdout), []byte(answer.Stderr), answer.Code
			var got, stderr bytes.Buffer
			code, handled := executeAsCLI(context.Background(), append([]string{"--state", filepath.Join(home, "go"), "--json"}, args...), &got, &stderr)
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: code, Stdout: got.String(), Stderr: stderr.String()})
			if !handled || code != pyCode || !bytes.Equal(got.Bytes(), want) || !bytes.Equal(stderr.Bytes(), pyStderr) {
				t.Errorf("python (%d) %s; go (%d) %s; stderr %s", pyCode, want, code, got.String(), stderr.String())
			}
		})
	}
}
