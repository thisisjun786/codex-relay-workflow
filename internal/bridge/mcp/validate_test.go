package mcp

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The fixture refused-arguments.json holds the calls; the golden holds, for each, the first
// line and the failing locations, which began as what Python's FastMCP server (mcp 1.30.0,
// pydantic 2.13) reported through the Python mcp client against `python -m
// codex_thread_bridge.server`. The first thirteen are the schema failures the round-1 check
// found; the last two are multi-field cases whose order a map-ordered validator could not keep.
type refusedCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func Test_a_refused_argument_reports_every_field_in_signature_order_the_same_way_every_time(t *testing.T) {
	var cases []refusedCall
	if err := json.Unmarshal(golden.Fixture(t, "refused-arguments.json"), &cases); err != nil {
		t.Fatal(err)
	}
	home, env := isolated(t)
	s := serve(t, []string{"--socket", filepath.Join(home, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	cwd := t.TempDir()
	for i, tc := range cases {
		encoded, err := json.Marshal(tc.Arguments)
		if err != nil {
			t.Fatal(err)
		}
		var arguments map[string]any
		if err := json.Unmarshal([]byte(strings.ReplaceAll(string(encoded), "${CWD}", cwd)), &arguments); err != nil {
			t.Fatal(err)
		}
		var first string
		for run := range 20 {
			result := call(t, s, tc.Tool, arguments)
			text := result.Content[0].(*sdk.TextContent).Text
			if run == 0 {
				first = text
			} else if text != first {
				t.Fatalf("case %d run %d changed:\n%s\nthen\n%s", i, run, first, text)
			}
			if !result.IsError {
				t.Fatalf("case %d: not an error: %s", i, text)
			}
		}
		lines := strings.Split(first, "\n")
		fields := []string{}
		for _, line := range lines[1:] {
			if line != "" && !strings.HasPrefix(line, " ") {
				fields = append(fields, line)
			}
		}
		golden.CheckJSON(t, fmt.Sprintf("case %d %s", i, tc.Tool), map[string]any{"prefix": lines[0], "fields": fields})
		// Each location is followed by its own indented reason.
		if len(lines) != 1+2*len(fields) {
			t.Errorf("case %d: %d lines for %d fields:\n%s", i, len(lines), len(fields), first)
		}
	}
	s.finish(t)
}
