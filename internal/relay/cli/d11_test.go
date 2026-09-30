package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

func TestD11SupervisorReportRecordedRefusalMatchesPythonCLI(t *testing.T) {
	if os.Getenv("D11_CLI_HELPER") != "" {
		var args []string
		if err := json.Unmarshal([]byte(os.Getenv("D11_CLI_ARGS")), &args); err != nil {
			os.Exit(99)
		}
		recordSupervisorReport = func(context.Context, *supervisor.Channel, supervisor.Obligation, string, *string, *string) (map[string]any, error) {
			return nil, supervisor.Refusal{Reason: "contradictory_observation", Detail: "record refusal"}
		}
		os.Exit(ExecuteAs(context.Background(), os.Getenv("D11_ARGV0"), args, os.Stdout, os.Stderr))
	}
	reading := filepath.Join(t.TempDir(), "reading.json")
	data := `{"schema":"reporting-observation/1","reportingState":"unreported","reason":"terminal_without_report","relationshipId":"rel-1","executionGeneration":1,"selectors":{"state":"/state","markerRoot":"/markers","workspace":"/work","assignment":"a","session":"s","turn":"turn-1"}}`
	if err := os.WriteFile(reading, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	// The recorder refuses, as the Python reference's supervision.record_report was made to
	// (DeliveryRefused CONTRADICTORY_OBSERVATION 'record refusal'): each invocation answers over
	// a home and store of its own.
	environment := func(home string) []string {
		return append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_REFUSE_LIVE_STATE=")
	}
	arguments := func(home string) []string {
		return []string{"--state", filepath.Join(home, "state"), "supervisor-report-recorded", "--observation", reading}
	}
	home := t.TempDir()
	env, args := environment(home), arguments(home)
	rawArgs, _ := json.Marshal(args)
	for _, invocation := range []struct{ name, argv0 string }{{"codex-session-relay", "codex-session-relay"}, {"crw relay", "crw relay"}} {
		var compared map[string]any
		t.Run(invocation.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestD11SupervisorReportRecordedRefusalMatchesPythonCLI$")
			cmd.Env = append(env, "D11_CLI_HELPER=1", "D11_ARGV0="+invocation.argv0, "D11_CLI_ARGS="+string(rawArgs))
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				exit, ok := err.(*exec.ExitError)
				if !ok {
					t.Fatal(err)
				}
				code = exit.ExitCode()
			}
			compared = map[string]any{"code": code, "stdout": out.String(), "stderr": stderr.String()}
		})
		if compared != nil {
			expectGolden(t, "supervisor-report-recorded "+invocation.name, compared, home, reading)
		}
	}
}
