package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func Test24ArgparsePython(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", home)
	t.Setenv("CODEX_HOME", home)
	t.Setenv("COLUMNS", "80")
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name        string
		abbreviated []string
	}{
		{"supervisor-select", []string{"--ev", "absent"}},
		{"supervisor-standing", []string{"--proj", "absent"}},
		{"supervisor-report-recorded", []string{"--ev", "absent"}},
		{"supervisor-stage", []string{"--ev", "absent"}},
		{"supervisor-show", []string{"--mess", "absent"}},
		{"supervisor-send", []string{"--mess", "absent"}},
		{"supervisor-read", []string{"--mess", "absent", "--tu", "t", "--pr", "p", "--a", "recipient"}},
		{"reporting-derive", []string{"--rel", "absent", "--gr", "0.5"}},
		{"reporting-show", []string{"--mark", "root", "--work", "workspace", "--ass", "assignment", "--sess", "session", "--tu", "turn"}},
		{"merge-evidence", []string{"--repo", "invalid", "--pull", "1"}},
	} {
		for _, mode := range []string{"help", "empty", "unknown", "abbreviation", "unknown-after-required", "missing-value"} {
			var tail []string
			switch mode {
			case "help":
				tail = []string{"--help"}
			case "unknown":
				tail = []string{"--unknown", "value"}
			case "abbreviation":
				tail = tc.abbreviated
			case "unknown-after-required":
				tail = append(append([]string{}, tc.abbreviated...), "--unknown", "value")
			case "missing-value":
				tail = []string{tc.abbreviated[0]}
			}
			args := append([]string{"--state", filepath.Join(home, "state"), tc.name}, tail...)
			// Test parsing independently from clocks and host I/O, but use each real
			// command's FlagSet. Accepted parses return their machine-consumed values.
			// Python's parse is recorded (see askPython), asked here in the test.
			script := `import contextlib,io,json,sys
from codex_session_relay.cli import build_parser
out,err=io.StringIO(),io.StringIO()
code=0
with contextlib.redirect_stdout(out),contextlib.redirect_stderr(err):
 try:
  a=build_parser().parse_args(json.loads(sys.argv[1]))
  del a.handler
  print(json.dumps(vars(a),sort_keys=True))
 except SystemExit as e: code=e.code
print(json.dumps(dict(code=code,out=out.getvalue(),err=err.getvalue())))`
			encoded, _ := json.Marshal(args)
			var py struct {
				Code     int
				Out, Err string
			}
			askPython(t, tc.name+"/"+mode, &py, func() (any, error) {
				cmd := exec.Command(filepath.Join(repo, ".venv/bin/python"), "-c", script, string(encoded))
				output, err := cmd.CombinedOutput()
				if err != nil {
					return nil, fmt.Errorf("Python: %v %s", err, output)
				}
				var answer any
				return answer, json.Unmarshal(output, &answer)
			}, home)
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				var out, stderr bytes.Buffer
				command := Command{}
				for _, c := range Commands {
					if c.Name == tc.name {
						command = c
						break
					}
				}
				flags := newArgparseFlags(command)
				given, code, done := parseRelayArgs("codex-session-relay", flags, tail, &out, &stderr)
				if !done {
					var want map[string]any
					if err = json.Unmarshal([]byte(py.Out), &want); err != nil {
						t.Fatalf("accepted Go parse but Python rejected: %s", py.Err)
					}
					for name := range given {
						key := strings.ReplaceAll(name, "-", "_")
						if name == "as" {
							key = "asserted_by"
						}
						value := flags.Lookup(name).Value.String()
						if n, ok := want[key].(float64); ok {
							var actual float64
							_ = json.Unmarshal([]byte(value), &actual)
							if actual != n {
								t.Errorf("--%s: Go=%s Python=%v", name, value, n)
							}
						} else if want[key] != value {
							t.Errorf("--%s: Go=%s Python=%v", name, value, want[key])
						}
					}
					return
				}
				if code != py.Code || out.String() != py.Out || stderr.String() != py.Err {
					t.Errorf("argparse diff\nGo exit=%d stdout=%q stderr=%q\nPython exit=%d stdout=%q stderr=%q", code, out.String(), stderr.String(), py.Code, py.Out, py.Err)
				}
				// Exercise the actual dispatcher as well on parser terminations.
				out.Reset()
				stderr.Reset()
				code = Execute(context.Background(), args, &out, &stderr)
				if code != py.Code || out.String() != py.Out || stderr.String() != py.Err {
					t.Errorf("dispatcher diff\nGo exit=%d stdout=%q stderr=%q\nPython exit=%d stdout=%q stderr=%q", code, out.String(), stderr.String(), py.Code, py.Out, py.Err)
				}
			})
		}
	}
}

func newArgparseFlags(command Command) *flag.FlagSet {
	flags := flag.NewFlagSet(command.Name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	command.Flags(flags)
	return flags
}
