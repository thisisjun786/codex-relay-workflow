package doctor

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func targetTestWrite(t *testing.T, root, rel, data string) {
	t.Helper()
	p := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func targetTestRoot(t *testing.T, command, windows, args string) string {
	t.Helper()
	root := t.TempDir()
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["./hooks/a.json"],"mcpServers":"./.mcp.json"}`)
	targetTestWrite(t, root, "hooks/a.json", `{"hooks":{"SessionStart":[{"hooks":[{"command":`+command+`,"commandWindows":`+windows+`}]}]}}`)
	targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"t":{"args":`+args+`}}}`)
	return root
}

func targetTestWant(t *testing.T, root string, want []TargetIssue) {
	t.Helper()
	got, err := ValidateManifestTargets(root)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("issues: %+v %v; want %+v", got, err, want)
	}
}

func TestManifestTargetsBChecks(t *testing.T) {
	t.Run("B1_empty_hook_and_mcp", func(t *testing.T) {
		root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/x.js\""`, `null`, `["./y.js"]`)
		targetTestWrite(t, root, "x.js", "")
		targetTestWrite(t, root, "y.js", "")
		targetTestWant(t, root, []TargetIssue{{TargetHook, "target is empty: x.js"}, {TargetMCP, "target is empty: ./y.js"}})
	})
	t.Run("B2_dotdot", func(t *testing.T) {
		root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/../escape.js\""`, `null`, `[]`)
		targetTestWant(t, root, []TargetIssue{{TargetHook, "target escapes plugin root: ../escape.js"}})
	})
	t.Run("B3_symlink", func(t *testing.T) {
		root := targetTestRoot(t, `"node \"${PLUGIN_ROOT}/linked/file.js\""`, `null`, `[]`)
		outside := t.TempDir()
		targetTestWrite(t, outside, "file.js", "file")
		if err := os.Symlink(outside, filepath.Join(root, "linked")); err != nil {
			t.Fatal(err)
		}
		targetTestWant(t, root, []TargetIssue{{TargetHook, "target escapes plugin root: linked/file.js"}})
	})
	t.Run("B3b_hook_document_escape", func(t *testing.T) {
		root := targetTestRoot(t, `null`, `null`, `[]`)
		targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":["../outside-hook.json"]}`)
		targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: ../outside-hook.json"}})
	})
	for _, tc := range []struct {
		name    string
		present []string
		want    []TargetIssue
	}{
		{"B4a", []string{"entry.js"}, []TargetIssue{{TargetHook, "hook references missing dist: launch.ps1"}}},
		{"B4b", []string{"launch.ps1"}, []TargetIssue{{TargetHook, "hook references missing dist: entry.js"}}},
		{"B4c", nil, []TargetIssue{{TargetHook, "hook references missing dist: launch.ps1"}, {TargetHook, "hook references missing dist: entry.js"}}},
		{"B4d", []string{"launch.ps1", "entry.js"}, []TargetIssue{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := targetTestRoot(t, `null`, `"powershell \"${PLUGIN_ROOT}\\launch.ps1\" \"${PLUGIN_ROOT}\\entry.js\""`, `[]`)
			for _, p := range tc.present {
				targetTestWrite(t, root, p, "data")
			}
			targetTestWant(t, root, tc.want)
		})
	}
}

func TestManifestTargetsLegacyAndMalformed(t *testing.T) {
	for _, tc := range []struct {
		rel, data string
		want      []TargetIssue
		kind      TargetKind
	}{
		{"hooks/a.json", "", []TargetIssue{{TargetHook, "manifest hook file missing: ./hooks/a.json"}}, ""},
		{".mcp.json", "", []TargetIssue{{TargetMCP, "manifest mcpServers file missing: ./.mcp.json"}}, ""},
		{"hooks/a.json", "{broken", nil, TargetHook}, {".mcp.json", "{broken", nil, TargetMCP},
	} {
		t.Run(tc.rel+tc.data, func(t *testing.T) {
			root := targetTestRoot(t, `null`, `null`, `[]`)
			if tc.data == "" {
				if err := os.Remove(filepath.Join(root, tc.rel)); err != nil {
					t.Fatal(err)
				}
			} else {
				targetTestWrite(t, root, tc.rel, tc.data)
			}
			got, err := ValidateManifestTargets(root)
			if tc.kind == "" {
				if err != nil || !reflect.DeepEqual(got, tc.want) {
					t.Fatalf("%+v %v", got, err)
				}
			} else {
				var parse *TargetParseError
				if !errors.As(err, &parse) || parse.Kind != tc.kind || parse.Path != filepath.Join(root, tc.rel) || errors.Unwrap(parse) == nil {
					t.Fatalf("parse kind/path: %v", err)
				}
			}
		})
	}
	root := targetTestRoot(t, `"${PLUGIN_ROOT}/x.js"`, `null`, `["x.js",7,"x.txt"]`)
	targetTestWant(t, root, []TargetIssue{{TargetHook, "hook references missing dist: x.js"}, {TargetMCP, "mcp server t references missing dist: x.js"}})
	targetTestWant(t, t.TempDir(), []TargetIssue{})
}

func TestManifestTargetsExactEscapeAndShapes(t *testing.T) {
	root := targetTestRoot(t, `"${PLUGIN_ROOT}/linked/missing.js"`, `null`, `[]`)
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "linked")); err != nil {
		t.Fatal(err)
	}
	targetTestWant(t, root, []TargetIssue{{TargetHook, "hook references missing dist: linked/missing.js"}})
	// A hooks or mcpServers member of the wrong type declares nothing; the oracle passed it over
	// silently, the port reports it (CRW-1152, port: fixed).
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":"ignored","mcpServers":[]}`)
	targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hooks must be an array of hook file paths: ignored"}, {TargetMCP, "manifest mcpServers must be a string file path: "}})
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":[null,7,true,{},["x","y"]]}`)
	targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hook file must be a string: null"}, {TargetHook, "manifest hook file must be a string: 7"}, {TargetHook, "manifest hook file must be a string: true"}, {TargetHook, "manifest hook file must be a string: [object Object]"}, {TargetHook, "manifest hook file must be a string: x,y"}})
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":[],"mcpServers":"/outside.json"}`)
	targetTestWant(t, root, []TargetIssue{{TargetMCP, "manifest mcpServers file escapes plugin root: /outside.json"}})
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"hooks":[".//hooks/a.json"]}`)
	targetTestWant(t, root, []TargetIssue{{TargetHook, "manifest hook file escapes plugin root: .//hooks/a.json"}})
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":".//.mcp.json"}`)
	targetTestWant(t, root, []TargetIssue{{TargetMCP, "manifest mcpServers file escapes plugin root: .//.mcp.json"}})
	targetTestWrite(t, root, ".codex-plugin/plugin.json", `{"mcpServers":"./.mcp.json"}`)
	targetTestWrite(t, root, "entry.js", "target")
	targetTestWrite(t, root, ".mcp.json", `{"mcpServers":{"t":{"args":[".//entry.js"]}}}`)
	targetTestWant(t, root, []TargetIssue{{TargetMCP, "target escapes plugin root: .//entry.js"}})
	root = targetTestRoot(t, `"${PLUGIN_ROOT}/dir.js"`, `null`, `[]`)
	if err := os.Mkdir(filepath.Join(root, "dir.js"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A directory reports a nonzero size and passed as a target; it is not a file (CRW-1152, port: fixed).
	targetTestWant(t, root, []TargetIssue{{TargetHook, "target is not a regular file: dir.js"}})
	if err := os.Remove(filepath.Join(root, "dir.js")); err != nil {
		t.Fatal(err)
	}
	targetTestWrite(t, root, "dir.js", "target")
	// The walk visits integer keys first, ascending, as Object.entries does. An integer key is not one
	// of the ten supported events, so each is a finding (CRW-1152, port: fixed) and the order shows
	// in the findings ahead of the supported event's target.
	targetTestWrite(t, root, "hooks/a.json", `{"hooks":{"SessionStart":[{"hooks":[{"command":"${PLUGIN_ROOT}/three.js"}]}],"2":[{"hooks":[{"command":"${PLUGIN_ROOT}/two.js"}]}],"1":[{"hooks":[{"command":"${PLUGIN_ROOT}/one.js"}]}]}}`)
	targetTestWant(t, root, []TargetIssue{{TargetHook, "hook event is not supported: 1"}, {TargetHook, "hook event is not supported: 2"}, {TargetHook, "hook references missing dist: three.js"}})
}

func TestManifestTargetRelativeRootAndJSSpace(t *testing.T) {
	root := targetTestRoot(t, `"${PLUGIN_ROOT}/x.js\u00a0${PLUGIN_ROOT}/y.js"`, `null`, `[]`)
	targetTestWrite(t, root, "x.js", "target")
	t.Chdir(root)
	targetTestWant(t, ".", []TargetIssue{{TargetHook, "hook references missing dist: y.js"}})
	targetTestWrite(t, root, "hooks/a.json", `null`)
	if _, err := ValidateManifestTargets("."); err == nil {
		t.Fatal("null hook document accepted")
	}
}
