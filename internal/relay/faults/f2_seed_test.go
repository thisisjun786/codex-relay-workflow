package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func TestF2SeededCLIOracle(t *testing.T) {
	goldenParent(t)
	for _, tc := range []struct {
		name string
		args func(string, string) []string
	}{
		{"fail_state", func(_, pub string) []string {
			return []string{"fault-fail", "--publication", pub, "--claim-token", "t", "--error", "e"}
		}},
		{"fail_stale", func(_, pub string) []string {
			return []string{"fault-fail", "--publication", pub, "--claim-token", "wrong", "--error", "e"}
		}},
		{"adopt", func(id, _ string) []string {
			return []string{"fault-adopt", "--fault", id, "--external-ref", "CRW-1", "--scope", `{"projectKey":"P"}`}
		}},
		{"adopt_conflict", func(id, _ string) []string {
			return []string{"fault-adopt", "--fault", id, "--external-ref", "CRW-1", "--scope", `{"workspace":"other","projectKey":"P"}`}
		}},
		{"move", func(id, _ string) []string {
			return []string{"fault-move", "--fault", id, "--scope", `{"projectKey":"Q"}`}
		}},
		{"move_workspace_conflict", func(id, _ string) []string {
			return []string{"fault-move", "--fault", id, "--scope", `{"workspace":"other"}`}
		}},
		{"update", func(id, _ string) []string {
			return []string{"fault-update", "--fault", id, "--op", "add_label", "--value", `"urgent"`}
		}},
		{"update_no_issue", func(id, _ string) []string {
			return []string{"fault-update", "--fault", id, "--op", "set_project", "--value", `"P"`}
		}},
		{"update_wrong_project", func(id, _ string) []string {
			return []string{"fault-update", "--fault", id, "--op", "set_project", "--value", `"Q"`}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, e := os.MkdirTemp("/dev/shm", "f2-seeded-")
			if e != nil {
				t.Fatal(e)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			goDir, pyDir := filepath.Join(home, "go"), filepath.Join(home, "py")
			observation := `{"schema":"fault-observation/1","product":"crw","faultClass":"report_omitted","severity":"broken","signature":{"turn":"f2-seed"},"occurrenceKey":"first","scope":{"projectKey":"P"}}`
			code, reply := cliCall(t, goDir, "fault-observe", "--observation", observation)
			if code != 0 {
				t.Fatal(reply)
			}
			id := reply["faultId"].(string)
			publication := publicationID(id, openRecord, triggerOpen)
			if tc.name == "fail_stale" {
				ctx := context.Background()
				state, e := store.Open(ctx, filepath.Join(goDir, "relay.sqlite3"), "")
				if e != nil {
					t.Fatal(e)
				}
				_, e = state.Q(ctx).ExecContext(ctx, "UPDATE fault_publications SET state='claimed',claim_token='token',attempts=1 WHERE publication_id=?", publication)
				if e != nil {
					t.Fatal(e)
				}
				if e = state.Close(); e != nil {
					t.Fatal(e)
				}
			}
			args := tc.args(id, publication)
			pythonCopy(t, goDir, pyDir)
			answer := pyCLIRun(t, home, "", append([]string{"--state", pyDir, "--json"}, args...), false, pyHomeEnv(home)...)
			want, pyCode := []byte(answer.Stdout), answer.Code
			var got, stderr bytes.Buffer
			goCode, handled := executeAsCLI(context.Background(), append([]string{"--state", goDir, "--json"}, args...), &got, &stderr)
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: goCode, Stdout: got.String()})
			if !handled || pyCode != goCode || !bytes.Equal(want, got.Bytes()) {
				t.Errorf("python (%d): %s\ngo (%d): %s\nstderr: %s", pyCode, want, goCode, got.String(), stderr.String())
			}
		})
	}
}
