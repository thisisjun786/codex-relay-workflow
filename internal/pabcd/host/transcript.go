package host

import (
	"os"
	"strings"
	"unicode/utf8"
)

// TailBytes is how much of a transcript's end is searched (TRANSCRIPT_SEARCH_BYTES).
const TailBytes = 65_536

// ContextPressureMarkers are the compaction and context-pressure recovery phrases, lowercase,
// matched as substrings.
func ContextPressureMarkers() []string {
	return []string{"compacted session handoff", "context window has been compacted", "conversation history has been summarized"}
}

// ReadTranscriptTail is the last maxBytes of the transcript at path, decoded as Node's
// Buffer.toString("utf8") does, or "" on any error: an unreadable transcript must not block Codex
// (fail open). The whole file is read before it is cut, as the oracle does.
func ReadTranscriptTail(path string, maxBytes int) string {
	if path == "" || maxBytes <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return decodeUTF8(data[max(0, len(data)-maxBytes):])
}

// decodeUTF8 replaces each maximal invalid subpart with one U+FFFD (the WHATWG decoder Node uses);
// utf8.DecodeRune alone would replace every byte.
func decodeUTF8(b []byte) string {
	runes := make([]rune, 0, len(b))
	for len(b) > 0 {
		r, n := utf8.DecodeRune(b)
		if r == utf8.RuneError && n == 1 {
			n = invalidSubpart(b)
		}
		runes, b = append(runes, r), b[n:]
	}
	return string(runes)
}

// invalidSubpart is the length of the lead byte of b and the continuation bytes that could still
// have completed it (Unicode table 3-7).
func invalidSubpart(b []byte) int {
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch lead := b[0]; {
	case lead >= 0xC2 && lead <= 0xDF:
		need = 1
	case lead == 0xE0:
		lo, need = 0xA0, 2
	case lead == 0xED:
		hi, need = 0x9F, 2
	case lead >= 0xE1 && lead <= 0xEF:
		need = 2
	case lead == 0xF0:
		lo, need = 0x90, 3
	case lead == 0xF4:
		hi, need = 0x8F, 3
	case lead >= 0xF1 && lead <= 0xF3:
		need = 3
	default:
		return 1
	}
	n := 1
	for ; n <= need && n < len(b) && b[n] >= lo && b[n] <= hi; n++ {
		lo, hi = 0x80, 0xBF
	}
	return n
}

// HasStageMarkerForPhase is whether the tail already carries the hook's stage marker for phase, in
// either emitted form: the directive head `[crw: PLAN]` or the compaction-immune header
// `[crw — P: PLAN]` (the oracle's `[codexclaw...` markers after name-substitution rule R23).
func HasStageMarkerForPhase(tail, phase string) bool {
	var label string
	switch phase {
	case "I":
		label = "INTERVIEW"
	case "P":
		label = "PLAN"
	case "A":
		label = "AUDIT"
	case "B":
		label = "BUILD"
	case "C":
		label = "CHECK"
	case "D":
		label = "DONE"
	default:
		return false
	}
	return strings.Contains(tail, "[crw: "+label+"]") || strings.Contains(tail, "[crw — "+phase+": "+label+"]")
}

// IsContextPressureTail is whether the tail shows a compaction or context-pressure recovery marker.
func IsContextPressureTail(tail string) bool {
	// JavaScript's toLowerCase uses full case mapping, which makes U+0130 "i" plus a combining dot.
	lower := strings.ToLower(strings.ReplaceAll(tail, "\u0130", "i\u0307"))
	for _, marker := range ContextPressureMarkers() {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}
