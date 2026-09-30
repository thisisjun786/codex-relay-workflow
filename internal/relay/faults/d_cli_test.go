package faults

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The independent Python process, not a duplicated Go expectation, defines the CLI bytes.
// FLT-30: all Part D handlers are reachable; FLT-34: listing and refusal bytes.
func TestDCLIOracle(t *testing.T) {
	goldenParent(t)
	cases := [][]string{
		{"fault-policy", "--product", "crw", "--limit", "1"},
		{"fault-limit", "--product", "crw", "--limit", "1"},
		{"fault-policy", "--product", "crw", "--fault-class", "report_omitted", "--severity", "broken", "--reason", "r", "--limit", "5"},
		{"fault-limit", "--product", "crw", "--kind", "open_record", "--max-count", "3", "--window", "60", "--after", "x"},
		{"fault-policy", "--product", "crw", "--fault-class", "report_omitted", "--severity", "broken", "--reason", "fixed"},
		{"fault-policy", "--product", "crw", "--fault-class", "not_registered", "--severity", "degraded", "--reason", "unknown"},
		{"fault-limit", "--product", "crw", "--kind", "notification", "--max-count", "0", "--window", "60"},
		{"fault-notification-reserve", "--owner", "relay-daemon"},
		{"fault-relink", "--limit", "0"},
		{"fault-notifications", "--limit", "0"},
		{"fault-attention"}, {"fault-relink"}, {"fault-notifications"},
		{"fault-notification-raise", "--fault", "missing", "--reason", "test"},
		{"fault-notification-reserve", "--owner", "worker"},
		{"fault-notification-ack", "--notification", "missing", "--token", "t", "--ref", "r"},
		{"fault-notification-fail", "--notification", "missing", "--token", "t", "--error", "e"},
		{"fault-notification-reconcile", "--notification", "missing", "--delivered", "no", "--ref", "r"},
	}
	for _, args := range cases {
		t.Run(args[0]+"_"+strings.Join(args[1:], "_"), func(t *testing.T) {
			home, err := os.MkdirTemp("/dev/shm", "fault-d-oracle-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(home) })
			pythonDir := filepath.Join(home, "python")
			goDir := filepath.Join(home, "go")
			answer := pyCLIRun(t, home, pythonDir, append([]string{"--state", pythonDir, "--json"}, args...), false, pyHomeEnv(home)...)
			want, pyCode := []byte(answer.Stdout), answer.Code
			goDir = oracleState(answer.Created, pythonDir, goDir)
			var got, stderr bytes.Buffer
			goCode, handled := executeAsCLI(context.Background(), append([]string{"--state", goDir, "--json"}, args...), &got, &stderr)
			if !handled {
				t.Fatal("unhandled")
			}
			neverCreated(t, pythonDir, goDir)
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: goCode, Stdout: got.String(), Created: created(t, goDir)})
			if pyCode != goCode || !bytes.Equal(want, got.Bytes()) {
				t.Errorf("python (%d): %s\ngo (%d): %s\nstderr: %s", pyCode, want, goCode, got.String(), stderr.String())
			}
		})
	}
}
