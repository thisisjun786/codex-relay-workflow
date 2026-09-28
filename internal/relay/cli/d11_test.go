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
	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	reading := filepath.Join(t.TempDir(), "reading.json")
	data := `{"schema":"reporting-observation/1","reportingState":"unreported","reason":"terminal_without_report","relationshipId":"rel-1","executionGeneration":1,"selectors":{"state":"/state","markerRoot":"/markers","workspace":"/work","assignment":"a","session":"s","turn":"turn-1"}}`
	if err := os.WriteFile(reading, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	const script = `
import sys
from codex_session_relay import cli, supervision
from codex_session_relay.errors import DeliveryRefused, RefusalReason
def refused(*a,**kw): raise DeliveryRefused(RefusalReason.CONTRADICTORY_OBSERVATION,'record refusal')
supervision.record_report=refused
raise SystemExit(cli.main(sys.argv[1:]))
`
	home := t.TempDir()
	env := append(os.Environ(), "HOME="+home, "XDG_STATE_HOME="+home+"/xdg", "XDG_CONFIG_HOME="+home+"/config", "XDG_DATA_HOME="+home+"/data", "CODEX_HOME="+home+"/codex", "CRW_ALLOW_LIVE_STATE=1", "PYTHONPATH="+filepath.Join(root, "packages/codex-session-relay/src"))
	args := []string{"--state", filepath.Join(home, "state"), "supervisor-report-recorded", "--observation", reading}
	cmd := exec.Command(filepath.Join(root, ".venv/bin/python"), append([]string{"-c", script}, args...)...)
	cmd.Env = env
	var pyOut, pyErr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &pyOut, &pyErr
	pyCode := 0
	if err := cmd.Run(); err != nil {
		pyCode = err.(*exec.ExitError).ExitCode()
	}
	rawArgs, _ := json.Marshal(args)
	for _, invocation := range []struct{ name, argv0 string }{{"codex-session-relay", "codex-session-relay"}, {"crw relay", "crw relay"}} {
		t.Run(invocation.name, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestD11SupervisorReportRecordedRefusalMatchesPythonCLI$")
			cmd.Env = append(env, "D11_CLI_HELPER=1", "D11_ARGV0="+invocation.argv0, "D11_CLI_ARGS="+string(rawArgs))
			var out, stderr bytes.Buffer
			cmd.Stdout, cmd.Stderr = &out, &stderr
			code := 0
			if err := cmd.Run(); err != nil {
				code = err.(*exec.ExitError).ExitCode()
			}
			if code != pyCode || out.String() != pyOut.String() || stderr.String() != pyErr.String() {
				t.Fatalf("byte diff\nGo exit=%d stdout=%q stderr=%q\nPython exit=%d stdout=%q stderr=%q", code, out.String(), stderr.String(), pyCode, pyOut.String(), pyErr.String())
			}
		})
	}
}
