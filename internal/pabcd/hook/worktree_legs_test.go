package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

func worktreeLeg(t *testing.T, id string) harness.Leg {
	t.Helper()
	for _, leg := range harness.Legs() {
		if leg.ID == id {
			if leg.Handle == nil {
				t.Fatalf("leg %s has no handler", id)
			}
			return leg
		}
	}
	t.Fatalf("no leg %s", id)
	return harness.Leg{}
}

// answerContext is the additionalContext of a leg's answer, which must be one hookSpecificOutput line for event.
func answerContext(t *testing.T, answer, event string) string {
	t.Helper()
	var out struct {
		Specific struct {
			Event   string `json:"hookEventName"`
			Context string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if !strings.HasSuffix(answer, "\n") || json.Unmarshal([]byte(answer), &out) != nil || out.Specific.Event != event {
		t.Fatalf("not the %s envelope: %q", event, answer)
	}
	return out.Specific.Context
}

// Both legs are wired to the port and answer through the hook envelope; the legs read the process environment, so the
// test sets it. A cwd made long with "/." passes the 32,000 units the envelope allows and is cut there.
func TestWorktreeLegsAnswerThroughTheEnvelope(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(home, ".codex", "worktrees", "7627", "repo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CRW_WORKTREE_ROOTS", "")
	start := worktreeLeg(t, "session-start-detecting-managed-worktree")
	prompt := worktreeLeg(t, "user-prompt-submit-guiding-worktree-rename")
	payload := func(fields map[string]any) harness.Call {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return harness.Call{Raw: string(raw)}
	}

	if ctx := answerContext(t, start.Handle(payload(map[string]any{"hook_event_name": "SessionStart", "cwd": checkout})), "SessionStart"); !strings.Contains(ctx, "WORKTREE-GUARD-01") {
		t.Errorf("SessionStart context: %q", ctx)
	}
	if ctx := answerContext(t, prompt.Handle(payload(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "s", "cwd": checkout, "prompt": "rename the worktree"})), "UserPromptSubmit"); !strings.Contains(ctx, "WORKTREE-GUARD-02") {
		t.Errorf("UserPromptSubmit context: %q", ctx)
	}
	if got := start.Handle(payload(map[string]any{"hook_event_name": "SessionStart", "cwd": "/tmp"})); got != "" {
		t.Errorf("an unmanaged cwd is answered: %q", got)
	}
	long := answerContext(t, start.Handle(payload(map[string]any{"hook_event_name": "SessionStart", "cwd": checkout + strings.Repeat("/.", 20000)})), "SessionStart")
	if !strings.HasSuffix(long, "\n\n[truncated]") || len(utf16.Encode([]rune(long))) > 32000 {
		t.Errorf("a long context is not cut: %d units, ends %q", len(utf16.Encode([]rune(long))), long[len(long)-40:])
	}
}
