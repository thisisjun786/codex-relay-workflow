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

// The fixture refused-arguments.json holds calls whose arguments the input schema refuses
// (a missing field, a value of another type, a value outside an enum, several at once). Each is
// refused as an error result before the tool runs, the same way every time; the golden holds
// each refusal's text.
type refusedCall struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments"`
}

func Test_a_refused_argument_is_an_error_result_the_same_way_every_time(t *testing.T) {
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
		golden.Check(t, fmt.Sprintf("case %d %s", i, tc.Tool), []byte(strings.ReplaceAll(first, cwd, "<CWD>")))
	}
	s.finish(t)
}

// A number field takes a string holding the number, which a model writing a call sends; a string
// that holds no number, or not an integer where one is asked, is refused by the schema.
func Test_a_number_written_as_a_string_is_read_as_the_number(t *testing.T) {
	home, env := isolated(t)
	s := serve(t, []string{"--socket", filepath.Join(home, "absent.sock"), "--state-dir", filepath.Join(home, "ledger")}, env)
	for _, tc := range []struct {
		arguments map[string]any
		refused   string
	}{
		{map[string]any{"limit": "5"}, ""},
		{map[string]any{"limit": " 7 "}, ""},
		{map[string]any{"limit": 5.0}, ""},
		{map[string]any{"limit": "5.0"}, ""},
		{map[string]any{"limit": "5.5"}, "invalid arguments for list_threads: limit must be an integer"},
		{map[string]any{"limit": "0.99999999999999999"}, "invalid arguments for list_threads: limit must be an integer"},
		{map[string]any{"limit": "1e2"}, "invalid arguments for list_threads: limit must be an integer"},
		{map[string]any{"limit": "five"}, "invalid arguments for list_threads: limit must be an integer"},
		{map[string]any{"limit": true}, "invalid arguments for list_threads: limit must be an integer"},
	} {
		text := call(t, s, "list_threads", tc.arguments).Content[0].(*sdk.TextContent).Text
		if tc.refused == "" && strings.HasPrefix(text, "invalid arguments") || tc.refused != "" && text != tc.refused {
			t.Errorf("%v: %s", tc.arguments, text)
		}
	}
	s.finish(t)
}
