package cli

import (
	"bytes"
	"context"
	"flag"
	"io"
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
			// Parsing is tested apart from clocks and host I/O, with each real command's
			// FlagSet: an accepted parse is checked by the values it consumed, a parser
			// termination by its answer and the dispatcher's.
			var compared map[string]any
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
					accepted := map[string]string{}
					for name := range given {
						key := strings.ReplaceAll(name, "-", "_")
						if name == "as" {
							key = "asserted_by"
						}
						value := flags.Lookup(name).Value.String()
						accepted[key] = value
					}
					compared = map[string]any{"accepted": accepted}
					return
				}
				parsed := map[string]any{"code": code, "stdout": out.String(), "stderr": stderr.String()}
				// Exercise the actual dispatcher as well on parser terminations.
				out.Reset()
				stderr.Reset()
				code = Execute(context.Background(), args, &out, &stderr)
				compared = map[string]any{"parsed": parsed, "dispatched": map[string]any{"code": code, "stdout": out.String(), "stderr": stderr.String()}}
			})
			if compared != nil {
				expectGolden(t, tc.name+"/"+mode, compared, home)
			}
		}
	}
}

func newArgparseFlags(command Command) *flag.FlagSet {
	flags := flag.NewFlagSet(command.Name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	command.Flags(flags)
	return flags
}
