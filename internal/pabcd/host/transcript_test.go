package host

import (
	"os"
	"path/filepath"
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

// "hasStageMarkerForPhase: matches both directive head and stage header", with the CRW marker
// spelling of name-substitution rule R23.
func TestHasStageMarkerForPhaseMatchesBothForms(t *testing.T) {
	directive := `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"[crw: PLAN]\nWrite a diff-level plan"}}`
	for _, c := range []struct {
		tail, phase string
		want        bool
	}{
		{directive, "P", true}, {"[crw — A: AUDIT]", "A", true}, {directive, "B", false}, {"", "P", false}, {"noise", "IDLE", false},
		{"[crw — A: PLAN]", "P", false}, {"[codexclaw: PLAN]", "P", false}, // the header names its own phase; the CXC spelling is not CRW's
	} {
		if got := HasStageMarkerForPhase(c.tail, c.phase); got != c.want {
			t.Errorf("HasStageMarkerForPhase(%q, %q) = %v", c.tail, c.phase, got)
		}
	}
	for phase, label := range map[string]string{"I": "INTERVIEW", "P": "PLAN", "A": "AUDIT", "B": "BUILD", "C": "CHECK", "D": "DONE"} {
		if !HasStageMarkerForPhase("[crw: "+label+"]", phase) || !HasStageMarkerForPhase("[crw — "+phase+": "+label+"]", phase) {
			t.Errorf("phase %s has no marker", phase)
		}
	}
}

// "isContextPressureTail: detects compaction recovery markers"
func TestIsContextPressureTailDetectsMarkers(t *testing.T) {
	for tail, want := range map[string]bool{
		"... Compacted Session Handoff ...": true, "the conversation history has been summarized to free": true,
		"ordinary transcript text": false, "": false,
		"compacted sess\u0130on handoff": false, // JavaScript lowercases U+0130 to i plus a combining dot
	} {
		if got := IsContextPressureTail(tail); got != want {
			t.Errorf("IsContextPressureTail(%q) = %v", tail, got)
		}
	}
	if len(ContextPressureMarkers()) != 3 {
		t.Error("the marker list changed")
	}
}
