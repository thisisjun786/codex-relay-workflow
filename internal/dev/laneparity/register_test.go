//go:build dev

package laneparity

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func registered(t *testing.T) (Manifest, []Registered, []Leg) {
	t.Helper()
	dir, legs := generated(t)
	manifest, got, err := ReadRegistered(dir)
	if err != nil {
		t.Fatal(err)
	}
	return manifest, got, legs
}

// Each tamper is a way a registration could be wrong and still look declared; the cell must say so.
func TestCheckRegistration_rejectsWhatIsNotDeclaredAsRequired(t *testing.T) {
	const leg = "pre-tool-use-guarding-managed-worktree-deletion"
	for _, c := range []struct {
		name    string
		tamper  func(m *Manifest, got []Registered) []Registered
		problem string
	}{
		{"missing hook", func(_ *Manifest, got []Registered) []Registered {
			return slices.DeleteFunc(got, func(r Registered) bool { return r.Leg == leg })
		}, "no registration starts this leg"},
		{"same leg, another eventName", func(_ *Manifest, got []Registered) []Registered {
			for i := range got {
				if got[i].Leg == leg {
					got[i].Event = "PostToolUse"
				}
			}
			return got
		}, "registered under event PostToolUse"},
		{"event token the leg is not registered for", func(_ *Manifest, got []Registered) []Registered {
			for i := range got {
				if got[i].Leg == leg {
					got[i].EventToken = "post-tool-use"
				}
			}
			return got
		}, "command passes event"},
		{"matcher drift", func(_ *Manifest, got []Registered) []Registered {
			for i := range got {
				if got[i].Leg == leg {
					got[i].Matcher = ".*"
				}
			}
			return got
		}, "matcher"},
		{"timeout drift", func(_ *Manifest, got []Registered) []Registered {
			for i := range got {
				if got[i].Leg == leg {
					got[i].Timeout = 30
				}
			}
			return got
		}, "timeout 30, expected 10"},
		{"status message not renamed", func(_ *Manifest, got []Registered) []Registered {
			for i := range got {
				if got[i].Leg == leg {
					got[i].Status = strings.Replace(got[i].Status, "(crw)", "(codexclaw)", 1)
				}
			}
			return got
		}, "statusMessage"},
		{"async", func(_ *Manifest, got []Registered) []Registered {
			for i := range got {
				if got[i].Leg == leg {
					got[i].Async = true
				}
			}
			return got
		}, "async"},
		{"registered twice", func(_ *Manifest, got []Registered) []Registered {
			for _, r := range got {
				if r.Leg == leg {
					return append(got, r)
				}
			}
			return got
		}, "registered 2 times"},
		{"a registration that starts no leg", func(_ *Manifest, got []Registered) []Registered {
			return append(got, Registered{File: "wiring/hooks/x.json", Event: "Stop", Command: "node dist/cli.js"})
		}, "starts no leg"},
		{"a leg outside the table", func(_ *Manifest, got []Registered) []Registered {
			return append(got, Registered{File: "wiring/hooks/x.json", Event: "Stop", Command: `"/x/crw" hook stop --leg stop-unknown`, Leg: "stop-unknown", EventToken: "stop"})
		}, "not in the table"},
		{"another plugin's name", func(m *Manifest, got []Registered) []Registered {
			m.Name = "codexclaw"
			return got
		}, `name is "codexclaw"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			manifest, got, legs := registered(t)
			got = c.tamper(&manifest, slices.Clone(got))
			rep := CheckRegistration(manifest, got, legs)
			if rep.OK {
				t.Fatal("the registration cell passed")
			}
			var all []string
			for _, l := range rep.Legs {
				all = append(all, l.Problems...)
			}
			all = append(append(all, rep.Problems...), rep.Extra...)
			if !strings.Contains(strings.Join(all, "\n"), c.problem) {
				t.Errorf("problems %q do not say %q", all, c.problem)
			}
		})
	}
}

func TestDeclared_leavesOutALegUnderTheWrongEventOrDeclaredTwice(t *testing.T) {
	_, got, legs := registered(t)
	const moved, twice = "pre-tool-use-guarding-managed-worktree-deletion", "stop-waking-on-background-completion"
	for i := range got {
		if got[i].Leg == moved {
			got[i].Event = "Stop"
		}
	}
	for _, r := range got {
		if r.Leg == twice {
			got = append(got, r)
			break
		}
	}
	declared := Declared(got, legs)
	if _, ok := declared[moved]; ok {
		t.Error("a leg declared under another event is run for its own event's payloads")
	}
	if _, ok := declared[twice]; ok {
		t.Error("a leg declared twice is run")
	}
	if len(declared) != 32 {
		t.Errorf("%d legs declared, want 32", len(declared))
	}
}

func TestReadRegistered_refusesAHookFileOutsideTheRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, ".codex-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".codex-plugin", "plugin.json"), []byte(`{"name":"crw","hooks":["../escape.json"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadRegistered(dir); err == nil || !strings.Contains(err.Error(), "leaves the plugin root") {
		t.Fatalf("err = %v", err)
	}
}

func TestReadRegistered_acceptsAManifestWithASingleHookPath(t *testing.T) {
	dir := t.TempDir()
	write := func(rel, body string) {
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(".codex-plugin/plugin.json", `{"name":"crw","hooks":"./h.json"}`)
	write("h.json", `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"\"/x/crw\" hook --plugin-launch; exit 0","timeout":10},{"type":"prompt","command":"ignored"}]}]}}`)
	_, got, err := ReadRegistered(dir)
	if err != nil || len(got) != 1 || got[0].Leg != CompletionLeg || got[0].Event != "Stop" {
		t.Fatalf("got %+v, err %v", got, err)
	}
}
