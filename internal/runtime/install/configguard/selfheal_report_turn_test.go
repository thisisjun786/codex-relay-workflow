package configguard

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/guidancerecord"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
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

// CRW-1180 (verification of 79c79c07, d1): the flag warning pairs a compact with a resume only on evidence from the session's transcript
// that the compact is the compaction of the resume's own turn; a later turn's compact says the warning although its prompt never reached
// the prompt hook, and the compaction of the resume's own turn adds nothing even when the prompt hook never ran.
func TestSelfHealReportCompactIsPairedByTheTranscriptOfItsTurn(t *testing.T) {
	home := selfHealReportTempHome(t)
	selfHealReportWriteConfig(t, home)
	selfHealReportFakeCodex(t, selfHealReportSoftOff)
	start := func(session, source, transcript string) string {
		t.Helper()
		out, code := selfHealReportRun(t, home, `{"hook_event_name":"SessionStart","session_id":"`+session+`","cwd":"/ws","source":"`+source+`","transcript_path":"`+transcript+`"}`)
		if code != 0 {
			t.Fatalf("exit = %d", code)
		}
		return out
	}
	warning := selfHealReportEnvelopePrefix + pyjson.Dumps(selfHealReportWarning, pyjson.Options{Compact: true, Unicode: true}) + "}}\n"
	missed := newPairTranscript(t).turn("t1", false)
	if got := start("s1", "resume", missed.path); got != warning {
		t.Fatalf("resume = %q", got)
	}
	guidancerecord.NoteUserPrompt(selfHealReportEnv(home), "s1", "t1")
	missed.said().end("t1").turn("t2", true)
	if got := start("s1", "compact", missed.path); got != warning {
		t.Fatalf("the compact of a later turn whose prompt the hook missed = %q, want the warning", got)
	}
	own := newPairTranscript(t).turn("t1", true)
	start("s2", "resume", own.path)
	own.said()
	if got := start("s2", "compact", own.path); got != "" {
		t.Fatalf("the compact of the resume's own turn repeated the warning: %q", got)
	}
}
