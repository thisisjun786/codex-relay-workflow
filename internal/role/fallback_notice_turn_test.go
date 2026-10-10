package role

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
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

// CRW-1180 (verification of 79c79c07, d1): a compact pairs with a resume only on evidence from the session's transcript that it is the
// compaction of the resume's own turn. A compact of a later turn says the notice although the prompt hook saw only the resume's turn
// (the later prompt never reached it), and the compaction of the resume's own turn stays silent even when the prompt hook never ran.
func TestFallbackNoticeHookCompactIsPairedByTheTranscriptOfItsTurn(t *testing.T) {
	_, env := fallbackTestEnv(t)
	run := func(session, source, transcript string) string {
		var out strings.Builder
		raw := `{"session_id":"` + session + `","source":"` + source + `","transcript_path":"` + transcript + `"}`
		if code := RunFallbackNoticeHook(context.Background(), strings.NewReader(raw), &out, env, func(data []byte) string { return string(data) }); code != 0 {
			t.Fatalf("exit %d", code)
		}
		return out.String()
	}
	whole := run("s", "startup", "")
	if whole == "" {
		t.Fatal("baseline notice is empty")
	}
	missed := newPairTranscript(t).turn("t1", false)
	if got := run("m", "resume", missed.path); got != whole {
		t.Fatalf("resume of a session never given the notice answered %q", got)
	}
	guidancerecord.NoteUserPrompt(env, "m", "t1")
	missed.said().end("t1").turn("t2", true)
	if got := run("m", "compact", missed.path); got != whole {
		t.Errorf("the compact of a later turn whose prompt the hook missed answered %q, want the notice", got)
	}
	own := newPairTranscript(t).turn("t1", true)
	if got := run("o", "resume", own.path); got != whole {
		t.Fatalf("resume answered %q", got)
	}
	own.said()
	if got := run("o", "compact", own.path); got != "" {
		t.Errorf("the compact of the resume's own turn repeated the notice: %q", got)
	}
}
