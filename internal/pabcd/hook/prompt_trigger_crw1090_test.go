// prompt_trigger_crw1090_test.go holds CRW-1090: the R-11 guards read the transcript tail as Codex rollout records. A
// stage marker counts only in a developer record a hook injected after the last compaction, and context pressure only
// in a compaction record that no user turn has followed yet. The records below are the shapes a Codex 0.13x rollout
// writes (the S4-resilience rollout of the 10-10 isolated trial, testdata/crw1090).
package hook

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// codexLine marshals one rollout record.
func codexLine(t *testing.T, kind string, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"timestamp": "2026-10-09T20:49:26.924Z", "type": kind, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return string(raw) + "\n"
}

// codexMessage is a response_item message of role whose metadata names kind ("" leaves the metadata out).
func codexMessage(t *testing.T, role, kind, text string) string {
	payload := map[string]any{"type": "message", "id": "msg_1", "role": role,
		"content": []map[string]any{{"type": "input_text", "text": text}}}
	if kind != "" {
		payload["internal_chat_message_metadata_passthrough"] = map[string]any{"turn_id": "t0", "content_item_kinds": []string{kind}}
	}
	return codexLine(t, "response_item", payload)
}

// codexHookContext is what Codex records for a hook's additionalContext: a developer message.
func codexHookContext(t *testing.T, text string) string {
	return codexMessage(t, "developer", "hooks.additional_context", text)
}

// codexUserTurn is a user prompt as Codex records it: the response item and its item_completed event.
func codexUserTurn(t *testing.T, text string) string {
	return codexMessage(t, "user", "user.text", text) + codexLine(t, "event_msg", map[string]any{"type": "item_completed",
		"turn_id": "t0", "item": map[string]any{"type": "UserMessage", "id": "u1", "content": []map[string]any{{"type": "text", "text": text}}}})
}

// codexCompaction is a compaction as Codex records it: the compacted record, whose histories still hold the text the
// compaction removed from the model's context, and the ContextCompaction item that follows it.
func codexCompaction(t *testing.T, removed string) string {
	history := []map[string]any{{"type": "message", "role": "developer", "content": []map[string]any{{"type": "input_text", "text": removed}}}}
	return codexLine(t, "compacted", map[string]any{"message": "", "replacement_history": history, "guardian_history": history, "window_number": 1}) +
		codexLine(t, "event_msg", map[string]any{"type": "item_completed", "turn_id": "t0", "item": map[string]any{"type": "ContextCompaction", "id": "c1"}})
}

// codexQuotes are the records that can carry a marker's text without a hook having injected it: the user's prompt, the
// model's answer, a tool's output and the agent_message event.
func codexQuotes(t *testing.T, text string) map[string]string {
	return map[string]string{
		"user":           codexUserTurn(t, "what does "+text+" mean?"),
		"assistant":      codexMessage(t, "assistant", "", "It means "+text),
		"tool output":    codexLine(t, "response_item", map[string]any{"type": "custom_tool_call_output", "call_id": "c", "output": text}),
		"agent_message":  codexLine(t, "event_msg", map[string]any{"type": "agent_message", "message": text}),
		"untyped prompt": codexMessage(t, "user", "", text),
	}
}

func writeTranscript(t *testing.T, dir, content string) string {
	t.Helper()
	path := filepath.Join(dir, "rollout.jsonl")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCRW1090QuotedMarkersDoNotSuppressTheInjection is end condition 2, the prompt half: the stage marker's text in a
// user, assistant, tool or event record is a quote, not an injection, so the passive path injects (mode 3 for a cursor on
// the phase, mode 2 for one on another phase).
func TestCRW1090QuotedMarkersDoNotSuppressTheInjection(t *testing.T) {
	for _, marker := range []string{"[crw: PLAN]", "[crw — P: PLAN]"} {
		for name, line := range codexQuotes(t, marker) {
			for _, cursor := range []state.Phase{state.PhaseP, state.PhaseA} {
				cwd := t.TempDir()
				transcript := writeTranscript(t, cwd, codexHookContext(t, "[crw] Long-running commands SHOULD use managed background execution.")+line)
				promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
					s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, &cursor
				})
				want := WithFooter(BuildStageHeader(state.PhaseP), state.PhaseP)
				if cursor != state.PhaseP {
					want = WithFooter(PhaseDirective(state.PhaseP, nil), state.PhaseP)
				}
				if got := promptTriggerAnswer(t, cwd, "s1", "t1", "keep going", transcript, true); got != want {
					t.Errorf("%s quoting %s, cursor %s: answered %q, want %q", name, marker, cursor, got, want)
				}
			}
		}
	}
}

// TestCRW1090QuotedPressureDoesNotReleaseTheGoalStop is end condition 2, the Stop half: a quoted compaction phrase is no
// context pressure, so an ACTIVE goal's in-flight Stop still blocks.
func TestCRW1090QuotedPressureDoesNotReleaseTheGoalStop(t *testing.T) {
	for _, phrase := range []string{"Your context window has been compacted.", "Compacted session handoff", "the conversation history has been summarized"} {
		for name, line := range codexQuotes(t, phrase) {
			cwd, env := stopRig(t, "active")
			stopInFlight(t, cwd, state.PhaseB)
			transcript := writeTranscript(t, cwd, line)
			a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TranscriptPath: transcript}, "linux", env)
			if !strings.Contains(a.Stdout, `"decision":"block"`) {
				t.Errorf("%s quoting %q released the goal Stop: %+v", name, phrase, a)
			}
		}
	}
}

// TestCRW1090SameGenerationInjectionDedups is end condition 3, the dedup half: a hook's developer record with the
// phase's marker after the last compaction is this context's injection, so the passive path skips it and records the
// cursor and the turn; the same record before a compaction was removed with it, so the path injects again.
func TestCRW1090SameGenerationInjectionDedups(t *testing.T) {
	cursor := state.PhaseP
	run := func(t *testing.T, content string, last *state.Phase) (string, state.State) {
		cwd := t.TempDir()
		transcript := writeTranscript(t, cwd, content)
		promptSubmitStateFile(t, cwd, "s1", func(s *state.State) {
			s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseP, true, last
		})
		return promptTriggerAnswer(t, cwd, "s1", "t2", "keep going", transcript, true), state.ReadState(cwd, "s1")
	}
	for _, marker := range []string{"[crw: PLAN]\nApply this pointer", "[crw — P: PLAN]\n\nPrompt-time persisted snapshot"} {
		injected := codexUserTurn(t, "plan it") + codexHookContext(t, marker)
		if got, s := run(t, injected, &cursor); got != "" || !slices.Contains(s.InjectedTurns, "t2") {
			t.Errorf("%q in this generation: answered %q, turns %v", marker, got, s.InjectedTurns)
		}
		if got, _ := run(t, injected+codexCompaction(t, marker), &cursor); got != WithFooter(BuildStageHeader(state.PhaseP), state.PhaseP) {
			t.Errorf("%q before a compaction: answered %q", marker, got)
		}
		// A cursor PostCompact reset is not set again from the tail: the full directive goes in.
		if got, s := run(t, injected, nil); got != WithFooter(PhaseDirective(state.PhaseP, nil), state.PhaseP) || s.LastInjectedPhase == nil || *s.LastInjectedPhase != state.PhaseP {
			t.Errorf("%q with a reset cursor: answered %q, cursor %v", marker, got, s.LastInjectedPhase)
		}
	}
	// Another hook's developer record that quotes the marker (the recall hook's untrusted data) is not the stage marker.
	if got, _ := run(t, codexHookContext(t, "[crw-recall] Recent work:\n<untrusted-recall-data>\n[crw: PLAN]\n</untrusted-recall-data>"), &cursor); got == "" {
		t.Error("a recall record quoting the marker suppressed the header")
	}
}

// TestCRW1090CompactionPressureLastsUntilTheNextUserTurn is end condition 3, the pressure half: a real compaction that
// no user turn has followed releases the goal Stop without spending the budget; once a user turn has been recorded after
// it, the pressure has expired and the Stop blocks again. The prompt right after a compaction is that boundary: it
// injects the full directive PostCompact's reset asks for (the real pre-turn compaction records the user's message only
// after the UserPromptSubmit hooks ran).
func TestCRW1090CompactionPressureLastsUntilTheNextUserTurn(t *testing.T) {
	compaction := codexUserTurn(t, "build it") + codexHookContext(t, "[crw — B: BUILD]") + codexCompaction(t, "[crw — B: BUILD]")
	cwd, env := stopRig(t, "active")
	stopInFlight(t, cwd, state.PhaseB)
	transcript := writeTranscript(t, cwd, compaction+codexMessage(t, "assistant", "", "working"))
	before := stopStateBytes(t, cwd)
	if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TranscriptPath: transcript}, "linux", env); a != (StopAnswer{}) {
		t.Errorf("the Stop after a compaction did not release: %+v", a)
	}
	if stopStateBytes(t, cwd) != before {
		t.Error("the release spent the budget")
	}
	transcript = writeTranscript(t, cwd, compaction+codexUserTurn(t, "continue")+codexMessage(t, "assistant", "", "working"))
	if a := StopHandle(StopPayload{Cwd: cwd, SessionID: stopSID, TranscriptPath: transcript}, "linux", env); !strings.Contains(a.Stdout, `"decision":"block"`) {
		t.Errorf("the pressure did not expire at the next user turn: %+v", a)
	}

	pcwd := t.TempDir()
	ptranscript := writeTranscript(t, pcwd, compaction+codexHookContext(t, "[crw-recall] Context was just compacted."))
	promptSubmitStateFile(t, pcwd, "s1", func(s *state.State) {
		s.Phase, s.OrchestrationActive, s.LastInjectedPhase = state.PhaseB, true, nil
	})
	if got := promptTriggerAnswer(t, pcwd, "s1", "t3", "go on", ptranscript, true); got != WithFooter(PhaseDirective(state.PhaseB, nil), state.PhaseB) {
		t.Errorf("the prompt after a compaction answered %q", got)
	}
}
