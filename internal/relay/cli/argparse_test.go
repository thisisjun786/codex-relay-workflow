package cli

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
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
			// argparse spec: an accepted parse is checked by the values it consumed, a parser
			// termination by its answer and the dispatcher's.
			var compared map[string]any
			t.Run(tc.name+"/"+mode, func(t *testing.T) {
				result := argparse.Parse(tc.name, tail)
				if !result.Help && result.Message == "" {
					accepted := map[string]string{}
					for name := range result.Given {
						key := strings.ReplaceAll(name, "-", "_")
						if name == "as" {
							key = "asserted_by"
						}
						accepted[key] = dispatch.Args{Parsed: result}.Text(name)
					}
					compared = map[string]any{"accepted": accepted}
					return
				}
				parsed := map[string]any{"code": 0, "stdout": argparse.Help("codex-session-relay", tc.name), "stderr": ""}
				if !result.Help {
					parsed = map[string]any{"code": 2, "stdout": "", "stderr": result.Error("codex-session-relay", tc.name)}
				}
				// Exercise the actual dispatcher as well on parser terminations.
				var out, stderr bytes.Buffer
				code := Execute(context.Background(), args, &out, &stderr)
				compared = map[string]any{"parsed": parsed, "dispatched": map[string]any{"code": code, "stdout": out.String(), "stderr": stderr.String()}}
			})
			if compared != nil {
				expectGolden(t, tc.name+"/"+mode, compared, home)
			}
		}
	}
}
