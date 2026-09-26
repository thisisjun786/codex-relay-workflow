package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// testdata/validation_python.json holds, for each call, the first line and the failing
// locations Python's FastMCP server (mcp 1.30.0, pydantic 2.13) reported, recorded through
// the Python mcp client against `python -m codex_thread_bridge.server`. The first thirteen
// are the schema failures the round-1 check found; the last two are multi-field cases whose
// order a map-ordered validator could not keep.
type recordedValidation struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
	Prefix    string         `json:"prefix"`
	Fields    []string       `json:"fields"`
}

func Test_a_refused_argument_reports_every_field_in_signature_order_the_same_way_every_time(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "validation_python.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cases []recordedValidation
	if err := json.Unmarshal(raw, &cases); err != nil {
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
		if lines[0] != tc.Prefix || strings.Join(fields, ",") != strings.Join(tc.Fields, ",") {
			t.Errorf("case %d %s\n got %q %v\nwant %q %v", i, tc.Tool, lines[0], fields, tc.Prefix, tc.Fields)
		}
		// Each location is followed by its own indented reason.
		if len(lines) != 1+2*len(tc.Fields) {
			t.Errorf("case %d: %d lines for %d fields:\n%s", i, len(lines), len(tc.Fields), first)
		}
	}
	s.finish(t)
}
