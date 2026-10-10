package host

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeTail(t *testing.T, content string) string {
	t.Helper()
	file := filepath.Join(t.TempDir(), "t.jsonl")
	if err := os.WriteFile(file, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return file
}

// transcript.test.ts "readTranscriptTail: missing/empty path -> ” (fail-open)"
func TestReadTranscriptTailMissingPathIsEmpty(t *testing.T) {
	for _, path := range []string{"", "/no/such/file/here.jsonl", t.TempDir()} { // a directory cannot be read either
		if got := ReadTranscriptTail(path, TailBytes); got != "" {
			t.Errorf("ReadTranscriptTail(%q) = %q", path, got)
		}
	}
}

// "readTranscriptTail: returns the byte-bounded tail", decoded as Buffer.toString("utf8") does: one
// U+FFFD per maximal invalid subpart (the expected strings were measured with Node 24).
func TestReadTranscriptTailIsByteBoundedAndDecodedLikeNode(t *testing.T) {
	text := writeTail(t, "AAAA\nBBBB\ntail-here")
	for max, want := range map[int]string{9: "tail-here", 1000: "AAAA\nBBBB\ntail-here", 0: "", -3: ""} {
		if got := ReadTranscriptTail(text, max); got != want {
			t.Errorf("max %d: %q, want %q", max, got, want)
		}
	}
	const x = "\uFFFD"
	for _, c := range []struct{ bytes, want string }{
		{"\xa9z", x + "z"}, {"a\xe2\x80", "a" + x}, {"\x80\x80", x + x}, {"\xf0\x9f\x98", x}, {"\xed\xa0\x80", x + x + x},
		{"\xc0\x80", x + x}, {"\xf4\x90\x80\x80", x + x + x + x}, {"\xef\xbf\xbd", x}, {"é—z", "é—z"},
	} {
		if got := ReadTranscriptTail(writeTail(t, c.bytes), TailBytes); got != c.want {
			t.Errorf("%q: %q, want %q", c.bytes, got, c.want)
		}
	}
	if got := ReadTranscriptTail(writeTail(t, "x\xc3\xa9y"), 2); got != x+"y" { // a byte cut through "é"
		t.Errorf("a cut through a rune: %q", got)
	}
}

// devRecord is a hook's additionalContext as a Codex rollout records it.
func devRecord(text string) string {
	return `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":` + strconv.Quote(text) +
		`}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["hooks.additional_context"]}}}` + "\n"
}

// "hasStageMarkerForPhase: matches both directive head and stage header", with the CRW marker
// spelling of name-substitution rule R23, now read from a hook's developer record (CRW-1090).
func TestHasStageMarkerForPhaseMatchesBothForms(t *testing.T) {
	for _, c := range []struct {
		tail, phase string
		want        bool
	}{
		{devRecord("[crw: PLAN]\nWrite a diff-level plan"), "P", true}, {devRecord("[crw — A: AUDIT]"), "A", true},
		{devRecord("[crw: PLAN]\nWrite a diff-level plan"), "B", false}, {"", "P", false}, {devRecord("noise"), "IDLE", false},
		{devRecord("[crw — A: PLAN]"), "P", false}, {devRecord("[codexclaw: PLAN]"), "P", false}, // the header names its own phase; the CXC spelling is not CRW's
		{"[crw: PLAN]\n", "P", false}, // raw text is no record
		{devRecord("[crw-recall] <untrusted-recall-data>[crw: PLAN]</untrusted-recall-data>"), "P", false}, // a quote inside another hook's context
	} {
		if got := ParseTranscriptGeneration(c.tail, true).HasStageMarkerForPhase(c.phase); got != c.want {
			t.Errorf("HasStageMarkerForPhase(%q, %q) = %v", c.tail, c.phase, got)
		}
	}
	for phase, label := range map[string]string{"I": "INTERVIEW", "P": "PLAN", "A": "AUDIT", "B": "BUILD", "C": "CHECK", "D": "DONE"} {
		g := ParseTranscriptGeneration(devRecord("[crw: "+label+"]")+devRecord("[crw — "+phase+": "+label+"]"), true)
		if !g.HasStageMarkerForPhase(phase) || !ParseTranscriptGeneration(devRecord("[crw — "+phase+": "+label+"]"), true).HasStageMarkerForPhase(phase) {
			t.Errorf("phase %s has no marker", phase)
		}
	}
}

// TestTranscriptGenerationReadsOnlyRecordsAfterTheLastCompaction: the records before the last compaction, the
// compacted record's own histories, the other roles, a record whose metadata names another kind and the cut first
// line of a window that does not start the file are not this context's injection (CRW-1090).
func TestTranscriptGenerationReadsOnlyRecordsAfterTheLastCompaction(t *testing.T) {
	const compacted = `{"type":"compacted","payload":{"message":"","replacement_history":[{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw: PLAN]"}]}],"guardian_history":["[crw — P: PLAN]"]}}` + "\n"
	const compactionItem = `{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"ContextCompaction"}}}` + "\n"
	const userTurn = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["user.text"]}}}` + "\n"
	const environment = `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["environments.environment_context"]}}}` + "\n"
	const otherKind = `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw: PLAN]"}],"internal_chat_message_metadata_passthrough":{"content_item_kinds":["host_skills.instructions"]}}}` + "\n"
	const legacyDev = `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw: PLAN]"}]}}` + "\n"
	const nullMetaDev = `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw: PLAN]"}],"internal_chat_message_metadata_passthrough":null}}` + "\n"
	const emptyKindsDev = `{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"[crw: PLAN]"}],"internal_chat_message_metadata_passthrough":{}}}` + "\n"
	for _, c := range []struct {
		name, tail      string
		whole           bool
		marker, pressed bool
	}{
		{"marker then compaction", devRecord("[crw: PLAN]") + compacted, true, false, true},
		{"marker then compaction item", devRecord("[crw: PLAN]") + compactionItem, true, false, true},
		{"compaction then marker", compacted + compactionItem + devRecord("[crw: PLAN]"), true, true, true},
		{"compaction then environment context", compacted + environment, true, false, true},
		{"compaction then user turn", compacted + userTurn + devRecord("[crw: PLAN]"), true, true, false},
		{"another kind of developer record", otherKind, true, false, false},
		// A developer record nothing attributes to a hook is not the marker (CRW-1090 fix round 1): the role alone does not say
		// who wrote it, and a missing marker only costs a re-injection, where a wrong one skips the directive.
		{"a developer record without metadata", legacyDev, true, false, false},
		{"a developer record with null metadata", nullMetaDev, true, false, false},
		{"a developer record with metadata that names no kind", emptyKindsDev, true, false, false},
		{"a cut first line", devRecord("[crw: PLAN]"), false, false, false},
		{"a cut first line before a whole one", "cut\n" + devRecord("[crw: PLAN]"), false, true, false},
		{"lines that are not records", "{bad\n[1]\n\"s\"\n" + devRecord("[crw: PLAN]"), true, true, false},
	} {
		g := ParseTranscriptGeneration(c.tail, c.whole)
		if g.HasStageMarkerForPhase("P") != c.marker || g.ContextPressure() != c.pressed {
			t.Errorf("%s: marker %v pressure %v, want %v %v", c.name, g.HasStageMarkerForPhase("P"), g.ContextPressure(), c.marker, c.pressed)
		}
	}
	// The quoted phrases the oracle matched are no pressure.
	for _, phrase := range []string{"Compacted Session Handoff", "the context window has been compacted", "conversation history has been summarized"} {
		if ParseTranscriptGeneration(`{"type":"event_msg","payload":{"type":"agent_message","message":"`+phrase+`"}}`+"\n", true).ContextPressure() {
			t.Errorf("%q read as pressure", phrase)
		}
	}
}

// ReadTranscriptGeneration reads the window from the file and fails open.
func TestReadTranscriptGenerationFailsOpenAndDropsTheCutLine(t *testing.T) {
	if g := ReadTranscriptGeneration("/no/such/file.jsonl", TailBytes); g.HasStageMarkerForPhase("P") || g.ContextPressure() {
		t.Error("a missing transcript read as a marker or pressure")
	}
	record := devRecord("[crw: PLAN]")
	file := writeTail(t, record)
	if !ReadTranscriptGeneration(file, len(record)).HasStageMarkerForPhase("P") {
		t.Error("a window that holds the whole file dropped its first record")
	}
	if ReadTranscriptGeneration(file, len(record)-1).HasStageMarkerForPhase("P") {
		t.Error("a window that cuts the record read it")
	}
}
