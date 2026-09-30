package cli_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func Test24_SCH_24_RealBinaryNeedsSocketBeforeAnyWrite(t *testing.T) {
	root := t.TempDir()
	binary := testsupport.CRW(t)
	state := filepath.Join(root, "state")
	for _, argv := range [][]string{{"supervisor-send", "--message", "m"}, {"supervisor-read", "--message", "m", "--turn", "t", "--proof", "p", "--as", "recipient"}} {
		args := append([]string{"relay", "--state", state}, argv...)
		cmd := exec.Command(binary, args...)
		cmd.Env = append(os.Environ(), "HOME="+root, "XDG_STATE_HOME="+root, "CODEX_HOME="+root)
		output, err := cmd.CombinedOutput()
		exit, ok := err.(*exec.ExitError)
		if !ok || exit.ExitCode() != 4 {
			t.Fatalf("%v exit %v output %s", args, err, output)
		}
		var answer map[string]any
		if err := json.Unmarshal(output, &answer); err != nil || answer["error"] != "usage" || !strings.Contains(answer["detail"].(string), "needs --socket:") {
			t.Fatalf("answer %v %v", answer, err)
		}
		// SCH-24 is about the command's own writes: the fence's _ownership_preflight admits (and
		// here initializes) the store before the handler refuses, and nothing is recorded in it.
		for _, table := range []string{"recipient_lifecycle", "supervisor_readbacks", "supervisor_attempts"} {
			if rows := snapshotQuery(t, filepath.Join(state, "relay.sqlite3"), "SELECT * FROM "+table); len(rows) != 0 {
				t.Fatalf("%v wrote %s: %v", args, table, rows)
			}
		}
	}
}
func Test24_SCH_25_DoctorClassifiesSupervisorCommands(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", root)
	var out, errOut bytes.Buffer
	code := cli.Execute(context.Background(), []string{"--state", filepath.Join(root, "state"), "doctor"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("doctor %d %s %s", code, out.String(), errOut.String())
	}
	if !strings.Contains(out.String(), "supervisor-send") || !strings.Contains(out.String(), "supervisor-read") || !strings.Contains(out.String(), "supervisor-stage") || !strings.Contains(out.String(), "supervisor-show") {
		t.Fatalf("classification %s", out.String())
	}
}
func Test24_SCH_26_GlobalSocketBeforeSubcommandAndStageShape(t *testing.T) {
	root := t.TempDir()
	t.Setenv("HOME", root)
	t.Setenv("XDG_STATE_HOME", root)
	state := filepath.Join(root, "state")
	cases := []struct {
		argv     []string
		exit     int
		contains string
	}{{[]string{"--state", state, "supervisor-stage", "--event", "abc", "--observation", "a.json"}, 4, "--observation"}, {[]string{"--state", state, "supervisor-stage", "--project", "PRJ-1", "--recipient", "01someone"}, 4, "--recipient"}, {[]string{"--state", state, "supervisor-stage", "--project", "PRJ-1", "--observation", "a.json"}, 4, "observation"}, {[]string{"--state", state, "supervisor-send", "--message", "m", "--socket", "/tmp/s"}, 2, "unrecognized arguments: --socket"}}
	for _, tc := range cases {
		var out, errOut bytes.Buffer
		code := cli.Execute(context.Background(), tc.argv, &out, &errOut)
		text := out.String() + errOut.String()
		if code != tc.exit || !strings.Contains(text, tc.contains) {
			t.Fatalf("argv %v code %d output %s", tc.argv, code, text)
		}
	}
}
