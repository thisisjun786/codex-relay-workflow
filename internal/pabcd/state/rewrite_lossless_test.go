package state

import (
	"strings"
	"testing"
)

// rewriteLosslessReceipt is a state file with one record whose receiptClaimed holds text exactly as the file stores it, escapes included.
func rewriteLosslessReceipt(text string) string {
	return rewriteFile("[" + rewriteRecord(`,"receiptClaimed":"`+text+`"`) + "]")
}

// The reader and the guard both decode with encoding/json, which reads an unpaired surrogate escape and each invalid UTF-8 byte
// as U+FFFD, so both sides rebuild the same text and a rewrite would write U+FFFD over what the file stores. The cases here
// feed the guard the file as stored and the rebuilt list of the real reader (rewriteVerdict).
func TestRewriteLosslessRefusesWhatTheDecoderWouldAlter(t *testing.T) {
	r255 := strings.Repeat("r", 255)
	for _, c := range []struct {
		name, file string
		keeps      bool
	}{
		{"a lone high surrogate escape", rewriteLosslessReceipt(`\ud800`), false},
		{"a lone low surrogate escape", rewriteLosslessReceipt(`\udc00`), false},
		{"a lone upper case surrogate escape", rewriteLosslessReceipt(`\uD83D`), false},
		{"a receipt the 256-unit cut of Node leaves on a high surrogate", rewriteLosslessReceipt(r255 + `\ud83d`), false},
		{"a high surrogate before an escaped backslash", rewriteLosslessReceipt(`\ud83d\\ude00`), false},
		{"a high surrogate before another escape", rewriteLosslessReceipt(`\ud83d\u0041`), false},
		{"a pair in the wrong order", rewriteLosslessReceipt(`\ude00\ud83d`), false},
		{"a lone surrogate after an escaped backslash", rewriteLosslessReceipt(`\\\ud800`), false},
		{"a lone surrogate in a key the reader ignores", `{"phase":"B","\ud800":1}`, false},
		{"a lone surrogate in the slug", `{"phase":"B","slug":"\ud800","unverifiedSubagents":[]}`, false},
		{"the bytes of a surrogate written as UTF-8", rewriteLosslessReceipt("\xed\xa0\x80"), false},
		{"an invalid byte", rewriteLosslessReceipt("\xff"), false},
		{"a cut multi-byte sequence", rewriteLosslessReceipt("\xe2\x82"), false},
		{"an invalid byte in a key", "{\"phase\":\"B\",\"a\xff\":1}", false},
		{"a valid pair", rewriteLosslessReceipt(`\ud83d\ude00`), true},
		{"a valid upper case pair", rewriteLosslessReceipt(`\uD83D\uDE00`), true},
		{"a pair that ends the longest receipt", rewriteLosslessReceipt(strings.Repeat("r", 254) + `\ud83d\ude00`), true},
		{"a pair after an escaped backslash", rewriteLosslessReceipt(`\\\ud83d\ude00`), true},
		{"an escaped backslash before the letters of a surrogate", rewriteLosslessReceipt(`\\ud800`), true},
		{"two escaped backslashes before the letters of a surrogate", rewriteLosslessReceipt(`\\\\ud800`), true},
		{"escapes that are no surrogate", rewriteLosslessReceipt(`\u00e9\u2028`), true},
		{"astral text written as UTF-8", rewriteLosslessReceipt("\U0001F600"), true},
		// The existing comparison, not the scanner, refuses a pair that the 256-unit cut splits; it stays as it was.
		{"a pair the 256-unit cut splits", rewriteLosslessReceipt(r255 + `\ud83d\ude00`), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rewriteVerdict(t, c.file); got != c.keeps {
				t.Errorf("RewriteKeepsUnverified = %v, want %v for %.120q", got, c.keeps, c.file)
			}
		})
	}
}

// The scan runs before the decoder, on any bytes, so it must stop at the end of the input wherever an escape is cut.
func TestRewriteLosslessScanNeverReadsPastTheInput(t *testing.T) {
	for _, raw := range []string{"", `\`, `\u`, `{"a":"x\`, `{"a":"\u12`, `{"a":"\uZZZZ"}`, `{"a":"\uZZZ\ud800"}`, `{"phase":"B"}\`} {
		if RewriteKeepsUnverified([]byte(raw), nil) {
			t.Errorf("a cut or malformed input was kept: %q", raw)
		}
	}
}

// An exponent is judged by its digits once the sign and the leading zeros are removed: 3e0000 is the value 3 that the reader keeps
// and the writer prints, so it is compared exactly, while 1e1000 stays a loss. The cap of 64 bytes holds.
func TestRewriteLosslessComparesExponentFormsOfOneValue(t *testing.T) {
	zeros := func(n int) string { return strings.Repeat("0", n) }
	for _, c := range []struct {
		attempts string
		keeps    bool
	}{
		{"3e0000", true},
		{"3E+000", true},
		{"3E+0000", true},
		{"3e-0000", true},
		{"30e-1", true},
		{"0e0000", true},
		{"3e0001", true},
		{"3e" + zeros(62), true},
		{"1e1000", false},
		{"1e01000", false},
		{"1e-0999", false},
		{"3e" + zeros(63), false},
	} {
		t.Run(c.attempts, func(t *testing.T) {
			file := rewriteFile("[" + rewriteRecord(`,"attempts":`+c.attempts) + "]")
			if got := rewriteVerdict(t, file); got != c.keeps {
				t.Errorf("RewriteKeepsUnverified = %v, want %v for attempts %s", got, c.keeps, c.attempts)
			}
		})
	}
}
