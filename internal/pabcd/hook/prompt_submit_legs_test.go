package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// promptSubmitLeg returns the wired UserPromptSubmit leg from the harness table.
func promptSubmitLeg(t *testing.T) harness.Leg {
	t.Helper()
	for _, leg := range harness.Legs() {
		if leg.ID == "user-prompt-submit-checking-pabcd-trigger" {
			if leg.Handle == nil {
				t.Fatal("the UserPromptSubmit leg has no handler")
			}
			return leg
		}
	}
	t.Fatal("no user-prompt-submit-checking-pabcd-trigger leg")
	return harness.Leg{}
}

// TestPromptSubmitLegAnswersThroughTheEnvelope drives the wired leg as the host does: the arming
// mandate comes back as one hookSpecificOutput line, the leg's quiet paths answer nothing, and the
// leg leaves the state the handler wrote. The legs read the process environment, so the test sets it.
func TestPromptSubmitLegAnswersThroughTheEnvelope(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_BIN", "{CRW}")
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(dir, "codex-home"))
	leg := promptSubmitLeg(t)
	if leg.Event != "user-prompt-submit" || leg.Slug != "user-prompt-submit" || leg.Stage != harness.Generic || leg.Recover || leg.SubagentExempt || leg.Gated {
		t.Errorf("the leg's registration changed: %+v", leg)
	}
	call := func(fields map[string]any, enabled bool) harness.Call {
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return harness.Call{Raw: string(raw), PabcdEnabled: enabled}
	}
	// The recorded loop_arm_request, replayed with the CRW name (CRW-1084, port: fixed): a project coordination request gets
	// the scope pointer for crw-run, where the oracle gave the implementation recipe; the pointer arms nothing.
	answer := leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t1", "prompt": "Start crw-loop for the migration project."}, true))
	ctx := answerContext(t, answer, "UserPromptSubmit")
	if !strings.HasPrefix(ctx, "[crw: LOOP — scope choice (ORCH-MANDATE-01)]") || strings.Contains(ctx, "loop init") {
		t.Errorf("the project coordination pointer: %q", ctx)
	}
	if s := state.ReadState(cwd, "rec-s1"); s.LoopArmSeen || len(s.InjectedTurns) != 1 {
		t.Errorf("the pointer must not record loopArmSeen: %+v", s)
	}
	// A single-task loop request keeps the recipe and records loopArmSeen.
	answer = leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t1b", "prompt": "Run crw-loop for this task"}, true))
	ctx = answerContext(t, answer, "UserPromptSubmit")
	if !strings.HasPrefix(ctx, "[crw: LOOP — orchestrate arming mandate (ORCH-MANDATE-01)]") {
		t.Errorf("the arming mandate: %q", ctx)
	}
	if s := state.ReadState(cwd, "rec-s1"); !s.LoopArmSeen {
		t.Errorf("the leg did not record loopArmSeen: %+v", s)
	}
	// A foreign event is not this handler's: the parse refuses it and the leg answers nothing.
	if got := leg.Handle(call(map[string]any{"hook_event_name": "Stop", "session_id": "rec-s1", "cwd": cwd}, true)); got != "" {
		t.Errorf("a foreign event answered %q", got)
	}
	// A prompt that asks for nothing answers nothing.
	if got := leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t2", "prompt": "Summarise the README."}, true)); got != "" {
		t.Errorf("a quiet prompt answered %q", got)
	}
	// PABCD off with a remember request: silent, but the marker is still recorded.
	if got := leg.Handle(call(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": "rec-s1", "cwd": cwd, "turn_id": "rec-t3", "prompt": "Remember this: the deploy key lives in the vault."}, false)); got != "" {
		t.Errorf("a pabcd-off prompt answered %q", got)
	}
	if s := state.ReadState(cwd, "rec-s1"); !s.MemoryWriteRequested {
		t.Errorf("the leg did not record the marker: %+v", s)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir, "rec-s1.json")); err != nil {
		t.Errorf("no state file: %v", err)
	}
}
