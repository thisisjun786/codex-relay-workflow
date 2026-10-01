package faults

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
)

type cliResult struct {
	code           int
	stdout, stderr string
}

func goFaultCLI(t *testing.T, args ...string) cliResult {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := executeAsCLI(context.Background(), args, &stdout, &stderr)
	return cliResult{code, stdout.String(), stderr.String()}
}

// Each fault command's help is its golden, one per command. What a line the parser cannot read
// answers is the relay parser's contract for every command (cmd/crw's
// TestRun_every_relay_command_line_has_the_usage_contract, internal/relay/argparse).
func TestFaultCommandsPrintTheirHelp(t *testing.T) {
	goldenParent(t)
	for _, name := range Names() {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			state := filepath.Join(home, "relay")
			t.Run("help", func(t *testing.T) {
				args := []string{"--json", name, "--help"}
				got := goFaultCLI(t, append([]string{"--state", state}, args...)...)
				if got.code != 0 || got.stderr != "" {
					t.Fatalf("exit %d: %s", got.code, got.stderr)
				}
				checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr, Created: created(t, state)})
			})
		})
	}
}

func TestFaultObserveMalformedJSON(t *testing.T) {
	goldenParent(t)
	for _, raw := range []string{"{not json", "[1,2"} {
		t.Run(raw, func(t *testing.T) {
			home := t.TempDir()
			args := []string{"--json", "fault-observe", "--observation", raw}
			got := goFaultCLI(t, append([]string{"--state", filepath.Join(home, "go", "relay")}, args...)...)
			checkGolden(t, "relay "+strings.Join(args, " "), args, runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr})
		})
	}
}

func TestFaultKindModuleRefusesAnUndeclaredNestedName(t *testing.T) {
	home := t.TempDir()
	args := []string{"--state", filepath.Join(home, "relay"), "--json", "--kind-module", "codex_session_relay.not_real", "fault-attention"}
	got := goFaultCLI(t, args...)
	checkGolden(t, "relay "+strings.Join(args[2:], " "), args[2:], runPathsOf(t, home), cliGolden{Code: got.code, Stdout: got.stdout, Stderr: got.stderr})
}
