package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview/ledger"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// sessionHookLeg returns the wired leg with the id from the harness table.
func sessionHookLeg(t *testing.T, id string) harness.Leg {
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

// TestSessionHookLegsAnswerThroughTheHarness wires the three D2a legs into the harness table and
// drives them as the host does: the bootstrap writes the state and its ignore file, the compaction
// resets only the reinjection cursor, and the interview capture records a round and reinjects the
// rescan directive in an interactive I phase. The legs read the process environment, so the test
// sets it.
func TestSessionHookLegsAnswerThroughTheHarness(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "ws")
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(dir, "codex"))
	t.Setenv("CRW_BIN", "{CRW}")
	call := func(event string, fields map[string]any) harness.Call {
		fields["hook_event_name"] = event
		raw, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return harness.Call{Raw: string(raw), PabcdEnabled: true}
	}

	start := sessionHookLeg(t, "session-start-bootstrapping-pabcd-state")
	if start.Event != "session-start" || start.Slug != "session-start" || start.Stage != harness.Generic || !start.Gated || start.SubagentExempt {
		t.Errorf("the bootstrap leg's registration changed: %+v", start)
	}
	if answer := start.Handle(call("SessionStart", map[string]any{"session_id": "s1", "cwd": cwd})); answer != "" {
		t.Errorf("the bootstrap answered %q", answer)
	}
	if _, err := os.Stat(state.StatePath(cwd, "s1")); err != nil {
		t.Errorf("no state file: %v", err)
	}
	if ignore, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, ".gitignore")); err != nil || string(ignore) != crwdir.GitignoreText {
		t.Errorf(".gitignore %q: %v", ignore, err)
	}
	// A payload the parse refuses (a non-canonical session id) writes nothing. The path a state file
	// would take for that id is the sanitised one under the state directory, so that is what is checked.
	if answer := start.Handle(call("SessionStart", map[string]any{"session_id": "../escape", "cwd": cwd})); answer != "" {
		t.Errorf("a refused session id answered %q", answer)
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName, "escape.json")); err == nil {
		t.Error("a refused session id wrote state")
	}
	if _, err := os.Stat(filepath.Join(cwd, crwdir.DirName, state.SessionsSubdir, "escape.json")); err == nil {
		t.Error("a refused session id wrote a state file")
	}

	compact := sessionHookLeg(t, "post-compact-resetting-reinject-cursor")
	if compact.Event != "post-compact" || compact.Slug != "post-compact" || compact.Stage != harness.Generic || !compact.Gated {
		t.Errorf("the compaction leg's registration changed: %+v", compact)
	}
	if _, err := state.EnsureState(cwd, "pc1"); err != nil {
		t.Fatal(err)
	}
	s := state.ReadState(cwd, "pc1")
	phase := state.PhaseB
	s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, &phase
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	if answer := compact.Handle(call("PostCompact", map[string]any{"session_id": "pc1", "cwd": cwd, "trigger": "auto"})); answer != "" {
		t.Errorf("the compaction answered %q", answer)
	}
	if got := state.ReadState(cwd, "pc1"); got.LastInjectedPhase != nil || got.Phase != state.PhaseB || !got.OrchestrationActive {
		t.Errorf("state after the compaction: %+v", got)
	}
	// The event the leg is not registered for is not the handler's: the parse refuses it.
	if answer := compact.Handle(call("Stop", map[string]any{"session_id": "pc1", "cwd": cwd})); answer != "" {
		t.Errorf("a foreign event answered %q", answer)
	}
	if got := state.ReadState(cwd, "pc1"); got.LastInjectedPhase != nil || got.Phase != state.PhaseB {
		t.Errorf("a foreign event mutated the state: %+v", got)
	}

	capture := sessionHookLeg(t, "post-tool-use-capturing-interview-answers")
	if capture.Event != "post-tool-use" || capture.Slug != "post-tool-use" || capture.Stage != harness.Generic || !capture.Gated || capture.SubagentExempt {
		t.Errorf("the interview leg's registration changed: %+v", capture)
	}
	round := map[string]any{
		"session_id": "s2", "cwd": cwd, "turn_id": "t1", "tool_name": "request_user_input",
		"tool_input":    map[string]any{"questions": []any{map[string]any{"id": "q1", "question": "Which?"}}},
		"tool_response": map[string]any{"answers": map[string]any{"q1": map[string]any{"answers": []any{"A"}}}},
	}
	if answer := capture.Handle(call("PostToolUse", round)); answer != "" {
		t.Errorf("an IDLE session reinjected: %q", answer)
	}
	if rows := ledger.ReadQaEvents(cwd, "s2"); len(rows) != 2 {
		t.Errorf("the round was not captured: %+v", rows)
	}
	if _, err := state.EnsureState(cwd, "s3"); err != nil {
		t.Fatal(err)
	}
	inphase := state.ReadState(cwd, "s3")
	inphase.Phase, inphase.OrchestrationActive = state.PhaseI, true
	if err := state.WriteState(cwd, inphase); err != nil {
		t.Fatal(err)
	}
	round["session_id"] = "s3"
	answer := capture.Handle(call("PostToolUse", round))
	if !strings.HasPrefix(answer, `{"hookSpecificOutput":{"hookEventName":"PostToolUse","additionalContext":`) || !strings.Contains(answer, "post-answer rescan") {
		t.Errorf("the I-phase answer: %q", answer)
	}
	if rows := ledger.ReadQaEvents(cwd, "s3"); len(rows) != 2 {
		t.Errorf("the round beside the reinjection: %+v", rows)
	}
	// Another tool is a no-op.
	other := map[string]any{"session_id": "s4", "cwd": cwd, "turn_id": "t1", "tool_name": "view_image"}
	if answer := capture.Handle(call("PostToolUse", other)); answer != "" || len(ledger.ReadQaEvents(cwd, "s4")) != 0 {
		t.Errorf("another tool: %q", answer)
	}
}
