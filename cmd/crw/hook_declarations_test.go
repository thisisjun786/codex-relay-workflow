package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// Every leg crw hook dispatches is declared on the activation surface, and every declaration names a
// leg it dispatches for the event the file registers it under: no component is left undeclared and no
// declaration starts nothing. The completion Stop is the one declaration without a leg.
func TestEveryDispatchedLegIsDeclared(t *testing.T) {
	plugin := filepath.Join("..", "..", "plugins", "crw")
	raw, err := os.ReadFile(filepath.Join(plugin, ".codex-plugin", "plugin.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Hooks []string `json:"hooks"`
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	const pointer = `"$HOME/.local/share/crw-runtime/current/bin/crw" hook `
	declared := map[string]string{} // leg -> event
	launches := 0
	for _, rel := range manifest.Hooks {
		raw, err := os.ReadFile(filepath.Join(plugin, rel))
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Hooks map[string][]struct {
				Hooks []struct {
					Command string `json:"command"`
				} `json:"hooks"`
			} `json:"hooks"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		for event, groups := range doc.Hooks {
			for _, g := range groups {
				for _, h := range g.Hooks {
					if h.Command == pointer+"--plugin-launch; exit 0" {
						launches++
						continue
					}
					words := strings.Fields(strings.TrimPrefix(h.Command, pointer))
					if !strings.HasPrefix(h.Command, pointer) || len(words) != 3 || words[1] != "--leg" || words[0] != kebab(event) {
						t.Errorf("%s: %s command %q", rel, event, h.Command)
						continue
					}
					if _, dup := declared[words[2]]; dup {
						t.Errorf("%s declared twice", words[2])
					}
					declared[words[2]] = words[0]
				}
			}
		}
	}
	dispatched := map[string]string{}
	for _, r := range componentHooks() {
		dispatched[r.ID] = r.Event
	}
	for _, l := range harness.Legs() {
		dispatched[l.ID] = l.Event
	}
	if launches != 1 || len(declared) != 33 || len(dispatched) != 33 {
		t.Fatalf("%d completion Stop, %d declared legs, %d dispatched legs", launches, len(declared), len(dispatched))
	}
	for leg, event := range dispatched {
		if declared[leg] != event {
			t.Errorf("%s (%s) is dispatched but declared as %q", leg, event, declared[leg])
		}
	}
	for leg := range declared {
		if _, ok := dispatched[leg]; !ok {
			t.Errorf("%s is declared but crw hook does not dispatch it", leg)
		}
	}
	if !slices.Contains(manifest.Hooks, "./wiring/hooks/pre-tool-use-guarding-github-post.json") {
		t.Error("the GitHub post guard is not declared")
	}
}

func kebab(s string) string {
	var b strings.Builder
	for i, r := range s {
		if r >= 'A' && r <= 'Z' {
			if i > 0 {
				b.WriteByte('-')
			}
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
