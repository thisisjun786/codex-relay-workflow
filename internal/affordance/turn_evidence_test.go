package affordance

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// pairTranscript is a session's Codex transcript for the CRW-1180 pair tests, in the record shapes of the isolated trial's codex 0.154.0
// rollouts: a turn that compacted before its SessionStart hooks ran, the answers the hooks gave, and the turns after it.
type pairTranscript struct {
	t    *testing.T
	path string
}

func newPairTranscript(t *testing.T) *pairTranscript {
	t.Helper()
	return &pairTranscript{t, filepath.Join(t.TempDir(), "rollout.jsonl")}
}

func (p *pairTranscript) add(records ...string) *pairTranscript {
	p.t.Helper()
	f, err := os.OpenFile(p.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		p.t.Fatal(err)
	}
	for _, record := range records {
		if _, err := f.WriteString(record + "\n"); err != nil {
			p.t.Fatal(err)
		}
	}
	if err := f.Close(); err != nil {
		p.t.Fatal(err)
	}
	return p
}

// turn is the start of a turn up to its context, with the compaction Codex ran before the SessionStart hooks when compacted is set.
func (p *pairTranscript) turn(id string, compacted bool) *pairTranscript {
	p.add(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"` + id + `"}}`)
	if compacted {
		p.add(`{"type":"compacted","payload":{"message":"","replacement_history":[]}}`,
			`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"`+id+`","item":{"type":"ContextCompaction","id":"c"}}}`,
			`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context></environment_context>"}]}}`)
	}
	return p.add(`{"type":"turn_context","payload":{"turn_id":"` + id + `"}}`)
}

// said is what Codex records of the SessionStart hooks' answers.
func (p *pairTranscript) said() *pairTranscript {
	return p.add(`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw] guidance"}]}}`)
}

// end is the turn's user prompt, its answer and its end.
func (p *pairTranscript) end(id string) *pairTranscript {
	return p.add(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go on"}]}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","turn_id":"`+id+`","item":{"type":"UserMessage","id":"u"}}}`,
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","turn_id":"`+id+`"}}`)
}

// CRW-1180 (verification of 79c79c07, d1): a compact pairs with a resume only on evidence that it is the compaction of the resume's own
// turn, and the evidence is the session's transcript. A compact of a later turn says the list when that turn's prompt never reached the
// prompt hook (an input over 4 MiB, which harness.ReadInput refuses before the hook can name the session).
func TestSessionStartCompactOfALaterTurnSaysTheListWhenItsPromptWasMissed(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	tr := newPairTranscript(t)
	src := func(source string) string {
		return payload(big, "SessionStart", "A", map[string]any{"source": source, "transcript_path": tr.path})
	}
	tr.turn("t1", false)
	if got := contextOf(t, hookAnswer(t, src("resume"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Fatalf("resume of a session never given the list: %q", got)
	}
	tr.said()
	userPrompt(t, big, "A", "t1", env)
	tr.end("t1")
	huge := payload(big, "UserPromptSubmit", "A", map[string]any{"turn_id": "t2", "prompt": strings.Repeat("x", 4<<20)})
	var out strings.Builder
	if code := RunHook(context.Background(), "user-prompt-submit", strings.NewReader(huge), &out, env, big); code != 0 {
		t.Fatalf("exit %d", code)
	}
	tr.turn("t2", true)
	if got := hookAnswer(t, src("compact"), big, env); !strings.Contains(got, "Loop contract") {
		t.Errorf("the compact of a later turn whose prompt the hook missed was silenced: %q", got)
	}
}

// The prompt hook ran once in the session and then stopped running (disabled, untrusted): that it ran once is no evidence of the turn, so
// a compact of a later turn says the list.
func TestSessionStartCompactOfALaterTurnSaysTheListWhenThePromptHookStopped(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	tr := newPairTranscript(t)
	src := func(source string) string {
		return payload(big, "SessionStart", "P", map[string]any{"source": source, "transcript_path": tr.path})
	}
	tr.turn("t0", false)
	hookAnswer(t, src("startup"), big, env)
	userPrompt(t, big, "P", "t0", env)
	tr.end("t0")
	// The resume is whole (the command changed); from here on the prompt hook does not run.
	other := func(k string) (string, bool) {
		if k == "CRW_BIN" {
			return "other-crw", true
		}
		return env(k)
	}
	tr.turn("t1", false)
	if got := contextOf(t, hookAnswer(t, src("resume"), big, other), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Fatalf("resume with another command: %q", got)
	}
	tr.said().end("t1").turn("t2", true)
	if got := hookAnswer(t, src("compact"), big, other); !strings.Contains(got, "Loop contract") {
		t.Errorf("the compact of a later turn after the prompt hook stopped was silenced: %q", got)
	}
}

// The compaction of the resume's own turn is told by the transcript, whether or not the prompt hook runs: Codex compacts the resumed
// session's first turn, then runs the resume's and the compact's SessionStart hooks, then the prompt hook. That compact adds nothing, once.
func TestSessionStartCompactOfTheResumesOwnTurnIsToldByTheTranscript(t *testing.T) {
	big := t.TempDir()
	seed(t, big, 40)
	env := sessionEnv(t, "crw")
	tr := newPairTranscript(t)
	src := func(source string) string {
		return payload(big, "SessionStart", "N", map[string]any{"source": source, "transcript_path": tr.path})
	}
	tr.turn("t1", true)
	if got := contextOf(t, hookAnswer(t, src("resume"), big, env), "SessionStart"); !strings.Contains(got, "Loop contract") {
		t.Fatalf("resume of a session never given the list: %q", got)
	}
	tr.said()
	if got := hookAnswer(t, src("compact"), big, env); got != "" {
		t.Errorf("the compact of the resume's own turn repeated the list: %q", got)
	}
	tr.said()
	if got := hookAnswer(t, src("compact"), big, env); !strings.Contains(got, "Loop contract") {
		t.Errorf("the next compact did not say the list: %q", got)
	}
}
