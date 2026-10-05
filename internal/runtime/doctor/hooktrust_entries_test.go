package doctor_test

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
)

// hookTrustEntriesRecorded is one case of testdata/hooktrust/entries-oracle.json, recorded by
// testdata/hooktrust/record-entries.mjs from CXC v0.2.40's dist (3c1459ac): a tree of files
// (text, or {"base64"} for bytes) and symlinks under one base directory, and the oracle's answer:
// its entries, the text of a structured error, or the class of an engine error (ENOENT, EISDIR,
// SyntaxError) whose text names a host path or the engine.
type hookTrustEntriesRecorded struct {
	Name         string                     `json:"name"`
	Key          string                     `json:"key"`
	RootName     string                     `json:"rootName"`
	RelativeRoot bool                       `json:"relativeRoot"`
	Files        map[string]json.RawMessage `json:"files"`
	Links        map[string]string          `json:"links"`
	Entries      []doctor.HookTrustEntry    `json:"entries"`
	Error        string                     `json:"error"`
	ErrorClass   string                     `json:"errorClass"`
}

func hookTrustEntriesRecordedCases(t *testing.T) []hookTrustEntriesRecorded {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hooktrust", "entries-oracle.json"))
	if err != nil {
		t.Fatal(err)
	}
	var recorded struct {
		Oracle string                     `json:"oracle"`
		Cases  []hookTrustEntriesRecorded `json:"cases"`
	}
	if err := json.Unmarshal(raw, &recorded); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recorded.Oracle, "3c1459ac") || len(recorded.Cases) < 190 {
		t.Fatalf("entries-oracle.json holds %d cases for oracle %q", len(recorded.Cases), recorded.Oracle)
	}
	return recorded.Cases
}

func hookTrustEntriesWrite(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

// hookTrustEntriesTree writes a recorded case's files and links under a fresh base directory.
func hookTrustEntriesTree(t *testing.T, recorded hookTrustEntriesRecorded) string {
	t.Helper()
	base := t.TempDir()
	for rel, raw := range recorded.Files {
		var content []byte
		var text string
		if json.Unmarshal(raw, &text) == nil {
			content = []byte(text)
		} else {
			var blob struct {
				Base64 string `json:"base64"`
			}
			if err := json.Unmarshal(raw, &blob); err != nil {
				t.Fatalf("file %s of %s: %v", rel, recorded.Name, err)
			}
			decoded, err := base64.StdEncoding.DecodeString(blob.Base64)
			if err != nil {
				t.Fatal(err)
			}
			content = decoded
		}
		hookTrustEntriesWrite(t, filepath.Join(base, filepath.FromSlash(rel)), content)
	}
	for rel, target := range recorded.Links {
		path := filepath.Join(base, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, path); err != nil {
			t.Fatal(err)
		}
	}
	return base
}

// hookTrustEntriesCheckResidual holds the cases the port answers differently on purpose: the
// matcher grammars of JavaScript and Go differ (docs/port-cxc/known-defects.md, port: kept). The
// oracle's answer stays in the fixture as evidence; the approved Go answer is the opposite one.
func hookTrustEntriesCheckResidual(t *testing.T, want hookTrustEntriesRecorded, got []doctor.HookTrustEntry, err error) {
	t.Helper()
	switch {
	case err != nil:
		t.Fatalf("ListHookTrustEntries: %v", err)
	case len(want.Entries) == 0 && (len(got) != 1 || got[0].Key != want.Key+":hooks/a.json:pre_tool_use:0:0"):
		t.Fatalf("JavaScript refuses the matcher and Go accepts it: want one entry, got %+v", got)
	case len(want.Entries) != 0 && len(got) != 0:
		t.Fatalf("JavaScript accepts the matcher and Go skips it: want no entry, got %+v", got)
	}
}

// TestListHookTrustEntries_recordedCases replays every recorded oracle answer: entries, the text
// of each structured error and the class of each engine error.
func TestListHookTrustEntries_recordedCases(t *testing.T) {
	for _, want := range hookTrustEntriesRecordedCases(t) {
		t.Run(want.Name, func(t *testing.T) {
			base := hookTrustEntriesTree(t, want)
			root := filepath.Join(base, "plugin")
			if want.RootName != "" {
				root = filepath.Join(base, want.RootName)
			}
			if want.RelativeRoot {
				t.Chdir(base)
				root = "plugin"
			}
			got, err := doctor.ListHookTrustEntries(root, want.Key)
			if err != nil && len(got) != 0 {
				t.Fatalf("entries %+v came with the error %v", got, err)
			}
			switch {
			case strings.HasPrefix(want.Name, "matcher_residual_"):
				hookTrustEntriesCheckResidual(t, want, got, err)
			case want.Error != "":
				if err == nil || err.Error() != want.Error {
					t.Fatalf("error = %v, want %q", err, want.Error)
				}
			case want.ErrorClass == "ENOENT" && !errors.Is(err, fs.ErrNotExist),
				want.ErrorClass == "EISDIR" && !errors.Is(err, syscall.EISDIR),
				want.ErrorClass != "" && err == nil:
				t.Fatalf("error = %v, want an engine error of class %s", err, want.ErrorClass)
			case want.ErrorClass != "":
			case err != nil:
				t.Fatalf("ListHookTrustEntries: %v", err)
			default:
				if want.Name == "ref_lone_surrogate_path" {
					// The fixture is read with encoding/json, which holds the surrogate as U+FFFD;
					// TestListHookTrustEntries_loneSurrogateReference asserts the exact key bytes.
					for i := range got {
						got[i].Key = strings.ToValidUTF8(got[i].Key, "\ufffd")
					}
				}
				if len(got) != len(want.Entries) || (len(got) > 0 && !reflect.DeepEqual(got, want.Entries)) {
					t.Fatalf("entries = %+v, want %+v", got, want.Entries)
				}
			}
		})
	}
}

// TestListHookTrustEntries_loneSurrogateReference: a reference holding a lone surrogate reaches
// the file system as U+FFFD (Node's encoding) while the key keeps the reference as written.
func TestListHookTrustEntries_loneSurrogateReference(t *testing.T) {
	root := t.TempDir()
	hookTrustEntriesWrite(t, filepath.Join(root, ".codex-plugin", "plugin.json"), []byte(`{"hooks":["hooks/\ud800.json"]}`))
	hookTrustEntriesWrite(t, filepath.Join(root, "hooks", "\ufffd.json"), []byte(`{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"x"}]}]}}`))
	got, err := doctor.ListHookTrustEntries(root, "fixture@market")
	if err != nil || len(got) != 1 || got[0].Key != "fixture@market:hooks/\xed\xa0\x80.json:stop:0:0" {
		t.Fatalf("ListHookTrustEntries = %+v, %v", got, err)
	}
}

// hookTrustEntriesPlugin is makePlugin of hook-trust.test.ts: a plugin whose manifest names ref and
// whose hook file holds document.
func hookTrustEntriesPlugin(t *testing.T, document any, ref string) string {
	t.Helper()
	root := t.TempDir()
	manifest, err := json.Marshal(map[string]any{"name": "fixture", "hooks": []string{ref}})
	if err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	hookTrustEntriesWrite(t, filepath.Join(root, ".codex-plugin", "plugin.json"), manifest)
	hookTrustEntriesWrite(t, filepath.Join(root, strings.TrimPrefix(ref, "./")), content)
	return root
}

func hookTrustEntriesCommand(command string, extra map[string]any) map[string]any {
	handler := map[string]any{"type": "command", "command": command}
	for key, value := range extra {
		handler[key] = value
	}
	return handler
}

func hookTrustEntriesKeys(t *testing.T, root string) []string {
	t.Helper()
	entries, err := doctor.ListHookTrustEntries(root, "fixture@market")
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, entry := range entries {
		keys = append(keys, entry.Key)
	}
	return keys
}

func hookTrustEntriesPreToolUse(groups ...any) map[string]any {
	return map[string]any{"hooks": map[string]any{"PreToolUse": groups}}
}

// The five tests below are hook-trust.test.ts:127-186, ported.

func TestListHookTrustEntries_skipsAsyncHandlersAndDerivesExactKeys(t *testing.T) {
	root := hookTrustEntriesPlugin(t, hookTrustEntriesPreToolUse(map[string]any{
		"matcher": "^tool$",
		"hooks":   []any{hookTrustEntriesCommand("echo sync", nil), hookTrustEntriesCommand("echo async", map[string]any{"async": true})},
	}), "./hooks/sample.json")
	want := []string{"fixture@market:hooks/sample.json:pre_tool_use:0:0"}
	if got := hookTrustEntriesKeys(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestListHookTrustEntries_skipsHandlersWhoseTypeIsNotCommand(t *testing.T) {
	root := hookTrustEntriesPlugin(t, hookTrustEntriesPreToolUse(map[string]any{
		"hooks": []any{map[string]any{"type": "prompt", "command": "ignored"}, hookTrustEntriesCommand("echo kept", nil)},
	}), "./hooks/sample.json")
	want := []string{"fixture@market:hooks/sample.json:pre_tool_use:0:1"}
	if got := hookTrustEntriesKeys(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestListHookTrustEntries_skipsEmptyAndWhitespaceOnlyCommands(t *testing.T) {
	root := hookTrustEntriesPlugin(t, hookTrustEntriesPreToolUse(map[string]any{
		"hooks": []any{hookTrustEntriesCommand("", nil), hookTrustEntriesCommand("  \t", nil), hookTrustEntriesCommand("echo kept", nil)},
	}), "./hooks/sample.json")
	want := []string{"fixture@market:hooks/sample.json:pre_tool_use:0:2"}
	if got := hookTrustEntriesKeys(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestListHookTrustEntries_skipsOnlyTheGroupWithAnInvalidMatcher(t *testing.T) {
	root := hookTrustEntriesPlugin(t, hookTrustEntriesPreToolUse(
		map[string]any{"matcher": "[", "hooks": []any{hookTrustEntriesCommand("echo invalid group", nil)}},
		map[string]any{"matcher": "^valid$", "hooks": []any{hookTrustEntriesCommand("echo valid group", nil)}},
	), "./hooks/sample.json")
	want := []string{"fixture@market:hooks/sample.json:pre_tool_use:1:0"}
	if got := hookTrustEntriesKeys(t, root); !reflect.DeepEqual(got, want) {
		t.Fatalf("keys = %v, want %v", got, want)
	}
}

func TestListHookTrustEntries_refusesManifestReferencesOutsideThePluginRoot(t *testing.T) {
	root := hookTrustEntriesPlugin(t, map[string]any{"hooks": map[string]any{}}, "./hooks/sample.json")
	hookTrustEntriesWrite(t, filepath.Join(root, "..", "outside-hook.json"), []byte(`{"hooks":{}}`))
	hookTrustEntriesWrite(t, filepath.Join(root, ".codex-plugin", "plugin.json"), []byte(`{"hooks":["../outside-hook.json"]}`))
	if _, err := doctor.ListHookTrustEntries(root, "fixture@market"); err == nil || !strings.Contains(err.Error(), "escapes plugin root") {
		t.Fatalf("error = %v, want one that says the reference escapes the plugin root", err)
	}
}
