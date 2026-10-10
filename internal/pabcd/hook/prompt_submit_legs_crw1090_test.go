// prompt_submit_legs_crw1090_test.go drives a synthetic reproduction of the S4 trial of CRW-1090 through the wired leg, as
// `crw hook user-prompt-submit --leg user-prompt-submit-checking-pabcd-trigger` runs it. The trial's own rollout and state
// are not vendored (AGENTS.md, POLICY.md: fixtures are synthetic): the records below keep the shapes and the sizes that
// produced the bug and carry invented ids, paths and text.
package hook_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/harness"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// s4Rollout is the rollout of the trial's session as it stood at the UserPromptSubmit after a compaction: a compacted
// record far longer than the 64 KiB tail, whose replacement history is padding and whose guardian history, near its end,
// still holds the stage header the compaction removed from the model's context; the ContextCompaction item; Codex's own
// developer and environment records; the hooks' additionalContext records of the new context, none of them the PLAN
// marker; and the prompt.
func s4Rollout(t *testing.T, sid, turn string) string {
	t.Helper()
	line := func(kind string, payload map[string]any) string {
		raw, err := json.Marshal(map[string]any{"timestamp": "2030-01-02T03:04:05.678Z", "type": kind, "payload": payload})
		if err != nil {
			t.Fatal(err)
		}
		return string(raw) + "\n"
	}
	text := func(role, kind, body string) string {
		return line("response_item", map[string]any{"type": "message", "role": role,
			"content": []map[string]any{{"type": "input_text", "text": body}},
			"internal_chat_message_metadata_passthrough": map[string]any{"turn_id": turn, "content_item_kinds": []string{kind}}})
	}
	message := func(body string) []map[string]any {
		return []map[string]any{{"type": "message", "role": "developer", "content": []map[string]any{{"type": "input_text", "text": body}}}}
	}
	var b strings.Builder
	// Codex writes the replacement history before the guardian history in the record; a map would sort them the other way.
	replacement, err := json.Marshal(message(strings.Repeat("synthetic earlier context. ", 6000)))
	if err != nil {
		t.Fatal(err)
	}
	guardian, err := json.Marshal(message("[crw — P: PLAN]\n\nPrompt-time persisted snapshot of the session at /synthetic/repo"))
	if err != nil {
		t.Fatal(err)
	}
	b.WriteString(`{"timestamp":"2030-01-02T03:04:05.678Z","type":"compacted","payload":{"message":"","replacement_history":` + string(replacement) +
		`,"guardian_history":` + string(guardian) + `,"window_number":2}}` + "\n")
	b.WriteString(line("event_msg", map[string]any{"type": "item_completed", "turn_id": turn, "item": map[string]any{"type": "ContextCompaction", "id": "c1"}}))
	b.WriteString(line("event_msg", map[string]any{"type": "token_count", "info": nil}))
	b.WriteString(text("developer", "permissions.instructions", strings.Repeat("synthetic permissions text. ", 200)))
	b.WriteString(text("user", "environments.environment_context", "<environment_context>\n  <cwd>/synthetic/repo</cwd>\n</environment_context>"))
	for _, body := range []string{"[crw-recall] Context was just compacted.", "[crw] Long-running commands SHOULD use managed background execution.", strings.Repeat("[crw] synthetic hook note. ", 120)} {
		b.WriteString(text("developer", "hooks.additional_context", body))
	}
	b.WriteString(text("user", "user.text", "Reply with one short sentence: what is the first word in notes.txt? No commands."))
	return b.String()
}

// TestCRW1090S4CompactionReinjectsThePlanDirective is end condition 1: the S4 reproduction (a state whose
// lastInjectedPhase PostCompact reset, and the rollout as it stood at that UserPromptSubmit) through the wired leg prints
// the full [crw: PLAN] directive and records lastInjectedPhase P. The stage header the compaction removed still sits in
// the compacted record's guardian history inside the 64 KiB tail, which the oracle read as an injected directive.
func TestCRW1090S4CompactionReinjectsThePlanDirective(t *testing.T) {
	dir := t.TempDir()
	cwd := filepath.Join(dir, "repo")
	const sid, turn = "s4-synthetic-session", "s4-synthetic-turn-5"
	transcript := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(transcript, []byte(s4Rollout(t, sid, turn)), 0o644); err != nil {
		t.Fatal(err)
	}
	// The reproduction reproduces: the oracle's raw search of the tail finds the header the compaction removed.
	if tail := host.ReadTranscriptTail(transcript, host.TailBytes); !strings.Contains(tail, "[crw — P: PLAN]") {
		t.Fatal("the synthetic rollout does not hold the removed stage header inside its 64 KiB tail")
	}
	if err := os.MkdirAll(cwd, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := state.EnsureState(cwd, sid); err != nil {
		t.Fatal(err)
	}
	s := state.ReadState(cwd, sid)
	s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, nil
	s.InjectedTurns, s.StopBlockTurnID = []string{"s4-synthetic-turn-1", "s4-synthetic-turn-2", "s4-synthetic-turn-3"}, new(string)
	*s.StopBlockTurnID = "s4-synthetic-turn-3"
	if err := state.WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_BIN", "{CRW}")
	t.Setenv("CODEX_SQLITE_HOME", filepath.Join(dir, "codex-home"))
	raw, err := json.Marshal(map[string]any{"hook_event_name": "UserPromptSubmit", "session_id": sid, "turn_id": turn, "cwd": cwd,
		"transcript_path": transcript, "prompt": "Reply with one short sentence: what is the first word in notes.txt? No commands."})
	if err != nil {
		t.Fatal(err)
	}
	answer := promptSubmitLeg(t).Handle(harness.Call{Raw: string(raw), PabcdEnabled: true})
	want := hook.WithFooter(hook.PhaseDirective(state.PhaseP, nil), state.PhaseP)
	if got := answerContext(t, answer, "UserPromptSubmit"); got != want {
		t.Errorf("the leg answered\n%q\nwant the full PLAN directive\n%q", got, want)
	}
	s = state.ReadState(cwd, sid)
	if s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseP || !slices.Contains(s.InjectedTurns, turn) {
		t.Errorf("the injection was not recorded: lastInjectedPhase %v, turns %v", s.LastInjectedPhase, s.InjectedTurns)
	}
}
