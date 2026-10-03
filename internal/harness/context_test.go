package harness

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
)

func tail(s string) string { return s[max(0, len(s)-40):] }

// The four buildContextOutput tests of the oracle (hook.test.ts:398-423), as exact bytes.
func TestContextOutputOracleCases(t *testing.T) {
	for _, c := range []struct{ name, event, ctx, want string }{
		{"envelope", "UserPromptSubmit", "hello", `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"hello"}}` + "\n"},
		{"CRLF and trim", "UserPromptSubmit", "  a\r\nb\r\n  ", `{"hookSpecificOutput":{"hookEventName":"UserPromptSubmit","additionalContext":"a\nb"}}` + "\n"},
		{"empty", "UserPromptSubmit", "", ""},
		{"only whitespace", "UserPromptSubmit", "   \r\n  ", ""},
		{"a lone CR is a line break", "Stop", "a\rb\r\rc\r\n\nd", `{"hookSpecificOutput":{"hookEventName":"Stop","additionalContext":"a\nb\n\nc\n\nd"}}` + "\n"},
	} {
		if got := ContextOutput(c.event, c.ctx); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	var parsed struct {
		HookSpecificOutput struct{ AdditionalContext string }
	}
	got := ContextOutput("UserPromptSubmit", strings.Repeat("x", 40000))
	if err := json.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatal(err)
	}
	if c := parsed.HookSpecificOutput.AdditionalContext; len(c) > MaxContext || !strings.HasSuffix(c, "[truncated]") {
		t.Errorf("a 40000-unit context: %d units, ends %q", len(c), tail(c))
	}
}

// What JavaScript does to a context that is long, holds astral characters, or begins and ends with
// characters trim() treats in its own way: each case is the byte length and SHA-256 of the output of
// buildContextOutput itself, run under Node 24 on the same input (the script is in the pull request).
func TestContextOutputMatchesTheOracleFunction(t *testing.T) {
	a, b := func(n int) string { return strings.Repeat("a", n) }, func(n int) string { return strings.Repeat("b", n) }
	for _, c := range []struct {
		name, event, ctx string
		size             int
		sha              string
	}{
		{"quote, backslash, control, U+2028/9, DEL, astral, <&> kept", "X\"y", "q\"\\\x01\u2028\u2029\x7f\U0001F600z<&>", 97, "32bb9883807ea13edf767d3ba9990581dbd8456438f4fa7f9223ef684beee609"},
		{"exactly 32000 units is not cut", "SessionStart", a(32000), 32079, "548afd513f7197cac9e99b1f181d9b113311f2486736a4019a54816bf2a2c8ea"},
		{"32001 units is cut at 31936", "SessionStart", a(32001), 32030, "9c40967c01b25d83f5b52fe767e0b7c6e915f6e465d761471cfebe4d91894602"},
		{"the cut splits an astral character: a lone surrogate escape", "SessionStart", a(31935) + "\U0001F600" + b(100), 32035, "7d8a5b10c32c784370127bd0bfe5e714450bb995eff1bde3d691e26ade78dcce"},
		{"the astral character ends before the cut and stays", "SessionStart", a(31934) + "\U0001F600" + b(100), 32032, "924ed7bb95e319631b1db82047cb5b54f2c620eb54f789f988cdc1b86dfd7251"},
		{"spaces and tabs before the cut are stripped", "SessionStart", a(31930) + strings.Repeat(" \t", 10) + b(100), 32024, "6667f9559dd2785705810736a88abdb6e3a3e4d43c8d70593d8c66a922717341"},
		{"length counts the text after trim", "SessionStart", "  " + strings.Repeat("x", 32005), 32030, "fc7de5b013843fcdf8131f365acc3b19e05eb84d34e9adc10f27cdd50ee3e571"},
		{"16000 astral characters are 32000 units", "SessionStart", strings.Repeat("\U0001F600", 16000), 64079, "444359728de8473fc4b822aed7d2a7d15c6bda7b593aa79f5b39882ce14ee4d5"},
		{"16001 are cut at 31936 units, between two characters", "SessionStart", strings.Repeat("\U0001F600", 16001), 63966, "a5fdf98092475d0e4d3d10e35fb6848bc8d46e3a489b08698bc001a7d64b8af4"},
		{"trim keeps U+0085", "Stop", "\u0085a\u0085", 76, "27e8a6e4d6022558bd9e6b53d9813a6f33da67b4d2742e45d5997b8a32a83ba7"},
		{"trim keeps U+180E", "Stop", "\u180ea\u180e", 78, "5edd9609247460fa35fbe3cc27e6c63ebe83a32ba70f71d7d42ca548b959b735"},
		{"only [ \\t\\r\\n] is stripped after the cut: a no-break space stays", "SessionStart", a(31935) + "\u00a0" + b(100), 32031, "ce4621f31c32560177f3cac53d04b0e8fe47080b161bf66a1356c18f8fea0b04"},
		{"trim strips U+FEFF and U+2028", "Stop", "\ufeff\u2028x\u2028\ufeff", 72, "ec7a1278c9b0a4e8ee1e0aa4a66dcd189da9c2a04a31d27c8c03866ddeb5f5d4"},
	} {
		got := ContextOutput(c.event, c.ctx)
		h := sha256.Sum256([]byte(got))
		if len(got) != c.size || hex.EncodeToString(h[:]) != c.sha {
			t.Errorf("%s: %d bytes %s, want %d bytes %s", c.name, len(got), hex.EncodeToString(h[:]), c.size, c.sha)
		}
	}
	// The one that cannot be a Go string: the text before the escape is kept, the escape is JSON.stringify's.
	cut := ContextOutput("SessionStart", a(31935)+"\U0001F600"+b(100))
	if !strings.HasSuffix(cut, `aaaa\ud83d\n\n[truncated]"}}`+"\n") || strings.Contains(cut, "\ufffd") {
		t.Errorf("a cut inside an astral character ends %q", tail(cut))
	}
}
