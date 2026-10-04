package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// The deletion guard's leg is wired to the port: it answers the PreToolUse deny envelope for the session's own checkout and
// stays silent for a neighbour and for a cwd outside the worktrees root. The leg reads the process environment.
func TestDeletionGuardLegAnswersThroughTheEnvelope(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	checkout := filepath.Join(home, ".codex", "worktrees", "zk3q", "repo")
	if err := os.MkdirAll(checkout, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, ".git"), []byte("gitdir: /fake\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, ".codex"))
	t.Setenv("CRW_WORKTREE_ROOTS", "")
	leg := worktreeLeg(t, "pre-tool-use-guarding-managed-worktree-deletion")
	if leg.Stage != harness.Guard || !leg.Recover || leg.Slug != "worktree-guard-pretool" {
		t.Errorf("the leg's registration changed: %+v", leg)
	}
	call := func(cwd, command string) harness.Call {
		raw, err := json.Marshal(map[string]any{"hook_event_name": "PreToolUse", "cwd": cwd, "tool_name": "Bash", "tool_input": map[string]any{"command": command}})
		if err != nil {
			t.Fatal(err)
		}
		return harness.Call{Raw: string(raw)}
	}
	out := leg.Handle(call(checkout, "rm -rf "+checkout))
	if !strings.HasPrefix(out, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"`) || !strings.HasSuffix(out, "\n") || !strings.Contains(out, "WORKTREE-GUARD-03") {
		t.Errorf("own checkout: %q", out)
	}
	if got := leg.Handle(call(checkout, "rm -rf "+filepath.Join(home, "elsewhere"))); got != "" {
		t.Errorf("a neighbour is answered: %q", got)
	}
	if got := leg.Handle(call(home, "rm -rf "+home)); got != "" {
		t.Errorf("an unmanaged cwd is answered: %q", got)
	}
}
