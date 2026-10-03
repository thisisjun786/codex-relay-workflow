package routing

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// Both entry points execute the production dispatcher and handlers. The test-only build
// overlay supplies the scenario's fixed clock; no timestamps or persisted JSON are normalized.
func Test23_BuiltCommandRoundTrips(t *testing.T) { binaryRoundTrips(t, "qa") }
func Test23_PR_18_BuiltProjectKind(t *testing.T) { binaryRoundTrips(t, "project-kind") }
func binaryRoundTrips(t *testing.T, mode string) {
	t.Parallel()
	builtBinary(t)
	// Each entry point runs the scenario against its own state directory and answers what the
	// one golden holds.
	for _, alias := range []bool{false, true} {
		state := filepath.Join(t.TempDir(), "state")
		var scenario cliScenario
		scenarioInputs(t, "cli-"+mode+".json", &scenario, [2]string{state, "<state>"})
		replies := make([]cliReply, len(scenario.Records))
		tables := make([]string, len(scenario.Records))
		if !t.Run(map[bool]string{false: "crw relay", true: "codex-session-relay"}[alias], func(t *testing.T) {
			for i, record := range scenario.Records {
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
				replies[i] = cliReply{code, stdout.String(), stderr.String()}
				s, err := store.Open(context.Background(), filepath.Join(state, "relay.sqlite3"), "")
				if err != nil {
					t.Fatal(err)
				}
				tables[i] = tablesJSON(t, s)
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
		}) {
			continue
		}
		opts := goldenPaths([2]string{state, "<state>"})
		trail := &tableTrail{}
		for i, record := range scenario.Records {
			key := fmt.Sprintf("%02d %s", i, record.Args[0])
			golden.CheckJSON(t, key+" reply", replies[i], opts...)
			trail.check(t, key+" tables", tables[i], opts...)
		}
	}
}
