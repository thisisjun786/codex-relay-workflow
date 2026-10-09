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

// shippedRoot is a scratch repository root holding what `crw-dev cxc hooks` reads and the completion Stop.
func shippedRoot(t *testing.T) string {
	t.Helper()
	src := repoRoot(t)
	root := t.TempDir()
	for _, rel := range []string{Declarations, Substitution, ShippedManifest, filepath.Join(ShippedHooksDir, CompletionStop)} {
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
	return root
}

// The generator publishes only inside the repository it was given (CRW-392): a declaration path that is
// a link, a dangling link, a directory, or a hooks directory that is a link, makes it refuse before it
// writes or removes anything, and nothing outside the repository changes.
func TestWriteShippedHooks_refuses_paths_that_leave_the_repository(t *testing.T) {
	const victim = "session-start-ensuring-provider-bridge.json"
	for name, setup := range map[string]func(t *testing.T, root, outside string){
		"declaration is a link to an existing file": func(t *testing.T, root, outside string) {
			if err := os.Symlink(outside, filepath.Join(root, ShippedHooksDir, victim)); err != nil {
				t.Fatal(err)
			}
		},
		"declaration is a dangling link": func(t *testing.T, root, outside string) {
			if err := os.Remove(outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, filepath.Join(root, ShippedHooksDir, victim)); err != nil {
				t.Fatal(err)
			}
		},
		"declaration is a directory": func(t *testing.T, root, outside string) {
			if err := os.Mkdir(filepath.Join(root, ShippedHooksDir, victim), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"hooks directory is a link": func(t *testing.T, root, outside string) {
			dir := filepath.Join(root, ShippedHooksDir)
			moved := filepath.Join(filepath.Dir(outside), "hooks-elsewhere")
			if err := os.Rename(dir, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, dir); err != nil {
				t.Fatal(err)
			}
		},
		"a parent of the hooks directory is a link": func(t *testing.T, root, outside string) {
			wiring := filepath.Join(root, "plugins", "crw", "wiring")
			moved := filepath.Join(filepath.Dir(outside), "wiring-elsewhere")
			if err := os.Rename(wiring, moved); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(moved, wiring); err != nil {
				t.Fatal(err)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			root := shippedRoot(t)
			outsideDir := t.TempDir()
			outside := filepath.Join(outsideDir, "config.toml")
			const precious = "model = \"keep\"\n"
			if err := os.WriteFile(outside, []byte(precious), 0o644); err != nil {
				t.Fatal(err)
			}
			stray := filepath.Join(root, ShippedHooksDir, "stray.json")
			if err := os.WriteFile(stray, []byte("{}\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			setup(t, root, outside)
			// The setup can move the directory that holds the stray file; resolve where it now is.
			strayNow, err := filepath.EvalSymlinks(stray)
			if err != nil {
				t.Fatal(err)
			}
			if err := WriteShippedHooks(root); err == nil {
				t.Fatal("WriteShippedHooks succeeded on a path that leaves the repository")
			}
			if got, err := os.ReadFile(outside); err == nil && string(got) != precious {
				t.Errorf("the file outside the repository was changed: %q", got)
			}
			if _, err := os.Stat(strayNow); err != nil {
				t.Errorf("a stray file was removed before the refusal: %v", err)
			}
			entries, _ := os.ReadDir(filepath.Dir(strayNow))
			for _, e := range entries {
				if e.Name() == "pre-tool-use-guarding-github-post.json" {
					t.Errorf("%s was written before the refusal", e.Name())
				}
			}
		})
	}
}
