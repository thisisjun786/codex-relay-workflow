//go:build dev

package cxccorpus

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// shippedDeclaration is one command hook a shipped file declares, read as the host reads it.
type shippedDeclaration struct {
	event, matcher, command, status string
	timeout                         int
}

func readShipped(t *testing.T, path string) []shippedDeclaration {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type          string `json:"type"`
				Command       string `json:"command"`
				Timeout       int    `json:"timeout"`
				StatusMessage string `json:"statusMessage"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
	var out []shippedDeclaration
	for event, groups := range doc.Hooks {
		for _, g := range groups {
			for _, h := range g.Hooks {
				if h.Type != "command" {
					t.Fatalf("%s: a %q hook", path, h.Type)
				}
				out = append(out, shippedDeclaration{event, g.Matcher, h.Command, h.StatusMessage, h.Timeout})
			}
		}
	}
	return out
}

// Every K1 leg ships as a declaration file of its own, read the way the host reads it: the leg's
// event, matcher and timeout as K1 records them, its statusMessage with "(codexclaw) " made
// "(crw) " (R24) and nothing else, and the command the runtime pointer's crw hook <event> --leg
// <leg>. The GitHub post guard ships beside them, and the manifest names the completion Stop and
// the 33 generated files, 34 declarations in all.
func TestShippedHooks_match_K1(t *testing.T) {
	root := repoRoot(t)
	decls, _, err := LoadDeclarations(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(decls.Legs) != 32 {
		t.Fatalf("K1 has %d legs", len(decls.Legs))
	}
	dir := filepath.Join(root, ShippedHooksDir)
	pointer := `"$HOME/.local/share/crw-runtime/current/bin/crw" hook `
	for _, d := range decls.Legs {
		got := readShipped(t, filepath.Join(dir, d.Leg+".json"))
		want := shippedDeclaration{d.Event, d.Matcher, pointer + Kebab(d.Event) + " --leg " + d.Leg,
			strings.Replace(d.Status, "(codexclaw) ", "(crw) ", 1), d.Timeout}
		if len(got) != 1 || got[0] != want {
			t.Errorf("%s: shipped %+v, want %+v", d.Leg, got, want)
		}
		if !strings.HasPrefix(d.Status, "(codexclaw) ") || strings.Contains(got[0].status, "codexclaw") {
			t.Errorf("%s: statusMessage %q from %q", d.Leg, got[0].status, d.Status)
		}
	}
	guard := readShipped(t, filepath.Join(dir, "pre-tool-use-guarding-github-post.json"))
	if len(guard) != 1 || guard[0] != (shippedDeclaration{"PreToolUse", "^(Bash|shell|exec_command|local_shell)$",
		pointer + "pre-tool-use --leg pre-tool-use-guarding-github-post", "(crw) Guarding GitHub posts", 10}) {
		t.Errorf("github post guard: %+v", guard)
	}
	var manifest struct {
		Hooks []string `json:"hooks"`
	}
	raw, err := os.ReadFile(filepath.Join(root, ShippedManifest))
	if err != nil || json.Unmarshal(raw, &manifest) != nil {
		t.Fatalf("manifest: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Hooks) != 34 || len(entries) != 34 || manifest.Hooks[0] != "./wiring/hooks/stop-recording-completion.json" {
		t.Fatalf("manifest names %d hook files, the directory holds %d: %q", len(manifest.Hooks), len(entries), manifest.Hooks)
	}
	for _, e := range entries {
		found := false
		for _, h := range manifest.Hooks {
			found = found || h == "./wiring/hooks/"+e.Name()
		}
		if !found {
			t.Errorf("%s ships but the manifest does not name it", e.Name())
		}
	}
	if problems := CheckShippedHooks(root); len(problems) > 0 {
		t.Errorf("%q", problems)
	}
}

// The equality check finds a drifted, a missing and a stray file and a manifest list out of order.
func TestCheckShippedHooks_reports_drift(t *testing.T) {
	src := repoRoot(t)
	root := t.TempDir()
	copyFile := func(rel string) {
		t.Helper()
		data, err := os.ReadFile(filepath.Join(src, rel))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	copyFile(Declarations)
	copyFile(Substitution)
	copyFile(ShippedManifest)
	copyFile(filepath.Join(ShippedHooksDir, CompletionStop))
	if err := WriteShippedHooks(root); err != nil {
		t.Fatal(err)
	}
	if problems := CheckShippedHooks(root); len(problems) > 0 {
		t.Fatalf("freshly written: %q", problems)
	}
	dir := filepath.Join(root, ShippedHooksDir)
	if err := os.WriteFile(filepath.Join(dir, "stop-checking-pabcd-continuation.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(dir, "pre-tool-use-guarding-github-post.json")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "extra.json"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := filepath.Join(root, ShippedManifest)
	raw, _ := os.ReadFile(manifest)
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	list := m["hooks"].([]any)
	list[1], list[2] = list[2], list[1]
	raw, _ = json.Marshal(m)
	if err := os.WriteFile(manifest, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(CheckShippedHooks(root), "\n")
	for _, fragment := range []string{"stop-checking-pabcd-continuation.json differs from", "pre-tool-use-guarding-github-post.json is missing",
		"extra.json is not generated from", "hooks must be, in this order"} {
		if !strings.Contains(got, fragment) {
			t.Errorf("no problem names %q:\n%s", fragment, got)
		}
	}
	if err := WriteShippedHooks(root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "extra.json")); !os.IsNotExist(err) {
		t.Errorf("the stray file stays: %v", err)
	}
}
