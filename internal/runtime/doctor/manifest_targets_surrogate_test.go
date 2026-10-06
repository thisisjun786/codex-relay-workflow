package doctor

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// targetSurrogateCase is a JSON escape sequence and the text Node's UTF-8 encoding gives a path that holds it:
// one U+FFFD for each lone surrogate, and a valid pair as its character.
type targetSurrogateCase struct{ name, escape, node string }

func targetSurrogateCases() []targetSurrogateCase {
	return []targetSurrogateCase{
		{"lone_high", `\ud800`, "\uFFFD"},
		{"lone_low", `\udc00`, "\uFFFD"},
		{"valid_pair", `\ud83d\ude00`, "\U0001F600"},
		{"two_adjacent_lone", `\ud800\ud800`, "\uFFFD\uFFFD"},
		{"low_then_high", `\udc00\ud800`, "\uFFFD\uFFFD"},
	}
}

// targetSurrogateDecode is the string the validator holds for a JSON string literal: pyjson.Loads keeps a lone
// surrogate escape as its three WTF-8 bytes.
func targetSurrogateDecode(t *testing.T, literal string) string {
	t.Helper()
	v, err := pyjson.Loads(literal, pyjson.LoadOptions{Surrogates: true, Numbers: pyjson.SpelledNumbers, Deep: true})
	s, ok := v.(string)
	if err != nil || !ok {
		t.Fatalf("decode %s: %v %v", literal, v, err)
	}
	return s
}

func TestManifestTargetsSurrogateNodeText(t *testing.T) {
	for _, tc := range targetSurrogateCases() {
		t.Run(tc.name, func(t *testing.T) {
			got := targetNodeText("a" + targetSurrogateDecode(t, `"`+tc.escape+`"`) + "b")
			if want := "a" + tc.node + "b"; got != want {
				t.Fatalf("%x; want %x", got, want)
			}
		})
	}
	// The one lone surrogate of the first case is exactly the bytes Buffer.from gives Node: 61 ef bf bd 62.
	if got := targetNodeText("a" + targetSurrogateDecode(t, `"\ud800"`) + "b"); got != "a\xef\xbf\xbdb" {
		t.Fatalf("%x", got)
	}
	for _, s := range []string{"", "plain", "é日本", "a\xffb", "\xed\xa0"} {
		if got := targetNodeText(s); got != s {
			t.Errorf("%q became %q", s, got)
		}
	}
}

func TestManifestTargetsSurrogateCommands(t *testing.T) {
	for _, tc := range targetSurrogateCases() {
		t.Run(tc.name, func(t *testing.T) {
			command := targetSurrogateDecode(t, `"x ${PLUGIN_ROOT}/bin/a`+tc.escape+`b"`)
			// The target keeps the original string; only the file-system look-up uses the U+FFFD spelling.
			want := []string{"bin/a" + targetSurrogateDecode(t, `"`+tc.escape+`"`) + "b"}
			if got := targetCommands(command); !reflect.DeepEqual(got, want) {
				t.Fatalf("targets %q (%x); want %q (%x)", got, got, want, want)
			}
		})
	}
	// A lone surrogate stays part of its target when JavaScript whitespace and a backslash separator follow it.
	command := targetSurrogateDecode(t, `"x ${PLUGIN_ROOT}/a\ud800\u00a0${PLUGIN_ROOT}\\b\udc00"`)
	want := []string{"a" + targetSurrogateDecode(t, `"\ud800"`), "b" + targetSurrogateDecode(t, `"\udc00"`)}
	if got := targetCommands(command); !reflect.DeepEqual(got, want) {
		t.Fatalf("targets %q; want %q", got, want)
	}
}

func TestManifestTargetsSurrogateCommandPaths(t *testing.T) {
	for _, tc := range targetSurrogateCases() {
		// The file is named with the U+FFFD Node's path encoding writes for a lone surrogate; the finding
		// keeps the original string.
		name := "bin/a" + tc.node + "b"
		original := "bin/a" + targetSurrogateDecode(t, `"`+tc.escape+`"`) + "b"
		command := `"x ${PLUGIN_ROOT}/bin/a` + tc.escape + `b"`
		t.Run(tc.name+"/present", func(t *testing.T) {
			root := targetTestRoot(t, command, `null`, `[]`)
			targetTestWrite(t, root, name, "data")
			targetTestWant(t, root, []TargetIssue{})
		})
		t.Run(tc.name+"/absent", func(t *testing.T) {
			root := targetTestRoot(t, command, `null`, `[]`)
			targetTestWant(t, root, []TargetIssue{{TargetHook, "hook references missing dist: " + original}})
		})
	}
}

func TestManifestTargetsSurrogateOtherPathSources(t *testing.T) {
	t.Run("manifest_hooks_entry", func(t *testing.T) {
		root := t.TempDir()
		original := targetSurrogateDecode(t, `"\ud800"`)
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/\ud800.json"]}`)
		targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hook file missing: ./hooks/" + original + ".json"}})
		targetTestWrite(t, root, "hooks/\uFFFD.json", `{"hooks":{}}`)
		targetTestWant(t, root, []TargetIssue{})
		targetTestWrite(t, root, "hooks/\uFFFD.json", "{broken")
		var parse *TargetParseError
		_, err := ValidateManifestTargets(root)
		if !errors.As(err, &parse) || parse.Kind != TargetHook || parse.Path != filepath.Join(root, "hooks", original+".json") {
			t.Fatalf("parse error: %v", err)
		}
	})
	t.Run("manifest_mcp_servers_file", func(t *testing.T) {
		root := t.TempDir()
		original := targetSurrogateDecode(t, `"\udc00"`)
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"./m-\udc00.json"}`)
		targetTestWant(t, root, []TargetIssue{{TargetMCP, "manifest mcpServers file missing: ./m-" + original + ".json"}})
		targetTestWrite(t, root, "m-\uFFFD.json", `{"mcpServers":{}}`)
		targetTestWant(t, root, []TargetIssue{})
	})
	t.Run("mcp_arg_and_server_name", func(t *testing.T) {
		root := t.TempDir()
		original := targetSurrogateDecode(t, `"\ud800"`)
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"./.mcp.json"}`)
		targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"s\ud800":{"args":["./dist/\ud83d\ude00\ud800.js"]}}}`)
		targetTestWant(t, root, []TargetIssue{{TargetMCP, "mcp server s" + original + " references missing dist: ./dist/\U0001F600" + original + ".js"}})
		targetTestWrite(t, root, "dist/\U0001F600\uFFFD.js", "data")
		targetTestWant(t, root, []TargetIssue{})
	})
	t.Run("non_string_hook_entry_message", func(t *testing.T) {
		root := t.TempDir()
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":[["a\ud800"]]}`)
		targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hook file must be a string: a" + targetSurrogateDecode(t, `"\ud800"`)}})
	})
}
