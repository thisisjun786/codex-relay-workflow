package harness

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

func TestContextSectionsPreserveRequiredGuidance(t *testing.T) {
	out := ContextOutputSections("SessionStart", []ContextSection{
		{Text: "binding: rec-s1", Required: true},
		{Text: strings.Repeat("long description 😀", 4000)},
		{Text: "snapshot: current task", Required: true},
		{Text: "gate: verify before mutation", Required: true},
	})
	var p struct {
		HookSpecificOutput struct{ AdditionalContext string }
	}
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatal(err)
	}
	body := p.HookSpecificOutput.AdditionalContext
	if len(utf16.Encode([]rune(body))) > MaxContext || !strings.Contains(body, "binding: rec-s1") || !strings.Contains(body, "snapshot: current task") || !strings.Contains(body, "gate: verify before mutation") || !strings.Contains(body, "[truncated]") || strings.ContainsRune(body, '\ufffd') {
		t.Fatal("required guidance lost or invalid context")
	}
}

func TestContextCutDoesNotLeaveASurrogate(t *testing.T) {
	out := ContextOutput("SessionStart", strings.Repeat("a", MaxContext-65)+"😀"+strings.Repeat("b", 100))
	if strings.Contains(out, `\ud83d`) || strings.ContainsRune(out, '\ufffd') || !strings.Contains(out, "[truncated]") {
		t.Fatalf("cut leaves invalid text: %q", tail(out))
	}
}

func TestContextSectionsRejectRequiredOverflow(t *testing.T) {
	out := ContextOutputSections("SessionStart", []ContextSection{
		{Text: strings.Repeat("required identity ", 3000), Required: true},
		{Text: "gate: do not bypass", Required: true},
	})
	if !strings.Contains(out, "required context exceeds") || strings.Contains(out, "required identity") || strings.Contains(out, "[truncated]") {
		t.Fatal("required overflow emitted a partial instruction")
	}
}

func TestObservationEmptyHomeUsesHomeFallback(t *testing.T) {
	for _, mode := range []string{"empty", "unset", "explicit"} {
		t.Run(mode, func(t *testing.T) {
			env, codex, _ := hookEnv(t)
			home := t.TempDir()
			t.Chdir(t.TempDir())
			env["HOME"] = home
			want := codex
			if mode != "explicit" {
				want = filepath.Join(home, ".codex")
				if mode == "empty" {
					env["CODEX_HOME"] = ""
				} else {
					delete(env, "CODEX_HOME")
				}
			}
			if !RecordInvocation(`{"session_id":"s1"}`, Component, "stop", lookup(env)) || len(records(t, want)) != 1 || len(records(t, ".")) != 0 {
				t.Fatal("observation escaped intended home")
			}
		})
	}
}

func TestObservationContainmentIncludesFilesystemRoot(t *testing.T) {
	for _, row := range []struct {
		root, path string
		want       bool
	}{
		{"/", "/.codex-plugin/plugin.json", true},
		{"/a/wt", "/a/wt/.codex-plugin/plugin.json", true},
		{"/a/wt", "/a/wt2/plugin.json", false},
		{"/a/wt", "/a/wt/../outside", false},
	} {
		if got := observationContains(row.root, row.path); got != row.want {
			t.Errorf("%s %s: %v", row.root, row.path, got)
		}
	}
}
