package hook_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
)

// The memory gate's leg is wired to the port: it answers the PreToolUse deny envelope for an unrequested note, stays silent for
// a command that writes nothing under the memories directory, and leaves the workspace as it found it. The leg reads the
// process environment.
func TestMemoryWriteLegAnswersThroughTheEnvelope(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "work")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(dir, "home"))
	t.Setenv("CODEX_HOME", filepath.Join(dir, "codex-home"))
	leg := worktreeLeg(t, "pre-tool-use-guarding-memory-write")
	if leg.Stage != harness.Guard || !leg.Recover || leg.Slug != "pre-tool-use-memory-write" {
		t.Errorf("the leg's registration changed: %+v", leg)
	}
	call := func(tool, input string) harness.Call {
		return harness.Call{Raw: `{"hook_event_name":"PreToolUse","session_id":"s1","turn_id":"t1","cwd":"` + cwd + `","tool_name":"` + tool + `","tool_input":` + input + `}`}
	}
	out := leg.Handle(call("memoriesadd_ad_hoc_note", `{"filename":"n.md"}`))
	if !strings.HasPrefix(out, `{"hookSpecificOutput":{"hookEventName":"PreToolUse","permissionDecision":"deny"`) || !strings.HasSuffix(out, "\n") || !strings.Contains(out, "MEMORY-WRITE-GATE") {
		t.Errorf("an unrequested note: %q", out)
	}
	if got := leg.Handle(call("Bash", `{"command":"ls"}`)); got != "" {
		t.Errorf("a command with no write: %q", got)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".crw")); err == nil {
		t.Error("the leg wrote into the workspace")
	}
}
