package routing

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Both entry points execute the production dispatcher and handlers. The test-only build
// overlay supplies the same clock as Python; no timestamps or persisted JSON are normalized.
func Test23_BuiltCommandRoundTrips(t *testing.T) { binaryRoundTrips(t, "qa") }
func Test23_PR_18_BuiltProjectKind(t *testing.T) { binaryRoundTrips(t, "project-kind") }
func binaryRoundTrips(t *testing.T, mode string) {
	builtBinary(t)
	for _, alias := range []bool{false, true} {
		t.Run(map[bool]string{false: "crw relay", true: "codex-session-relay"}[alias], func(t *testing.T) {
			state := filepath.Join(t.TempDir(), "state")
			raw := oracleScript(t, "cli_capture.py", mode, state)
			var records []struct {
				Args                   []string
				Code                   int
				Stdout, Stderr, Tables string
			}
			if err := json.Unmarshal(raw, &records); err != nil {
				t.Fatal(err)
			}
			if err := os.RemoveAll(state); err != nil {
				t.Fatal(err)
			}
			for _, record := range records {
				path := clockBinary
				argv := []string{"relay", "--state", state}
				if alias {
					path = filepath.Join(filepath.Dir(clockBinary), "codex-session-relay")
					argv = []string{"--state", state}
				}
				cmd := exec.Command(path, append(argv, record.Args...)...)
				var stdout, stderr bytes.Buffer
				cmd.Stdout, cmd.Stderr = &stdout, &stderr
				code := 0
				if err := cmd.Run(); err != nil {
					if exit, ok := err.(*exec.ExitError); ok {
						code = exit.ExitCode()
					} else {
						t.Fatal(err)
					}
				}
				if code != record.Code || stdout.String() != record.Stdout || stderr.String() != record.Stderr {
					t.Fatalf("built %s differs exit Python=%d Go=%d\nPython: %s\nGo: %s\nstderr: %s", record.Args[0], record.Code, code, record.Stdout, stdout.String(), stderr.String())
				}
				s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
				if err != nil {
					t.Fatal(err)
				}
				got, err := tablesJSON(context.Background(), s)
				closeErr := s.Close()
				if err != nil {
					t.Fatal(err)
				}
				if closeErr != nil {
					t.Fatal(closeErr)
				}
				if got != record.Tables {
					var want, actual any
					d := json.NewDecoder(strings.NewReader(record.Tables))
					d.UseNumber()
					if err := d.Decode(&want); err != nil {
						t.Fatal(err)
					}
					d = json.NewDecoder(strings.NewReader(got))
					d.UseNumber()
					if err := d.Decode(&actual); err != nil {
						t.Fatal(err)
					}
					t.Fatal(firstDifference("tables", want, actual))
				}
			}
		})
	}
}
