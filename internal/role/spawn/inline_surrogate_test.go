package spawn

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A lone surrogate reaches a Go string as the three WTF-8 bytes pyjson keeps for it: the spawn
// hook reads its payload with pyjson.Loads and Surrogates (hook_test.go:153), and the oracle
// lowercases a link folder with String.prototype.toLowerCase, which leaves a lone surrogate as
// it is. A folder name differing only in which surrogate it holds must therefore stay a key of
// its own. On dev both names lower to crw- plus three U+FFFD and collapse into one key.
func TestSpawnInlineMentionedFoldersKeepsLoneSurrogateNames(t *testing.T) {
	// Provenance: pyjson reads the JSON escapes \ud800 and \ud801 as exactly these bytes.
	for _, c := range []struct{ escape, want string }{
		{"x\\ud800y", "x\xed\xa0\x80y"},
		{"x\\ud801y", "x\xed\xa0\x81y"},
	} {
		value, err := pyjson.Loads("\""+c.escape+"\"", pyjson.LoadOptions{Python: true, Surrogates: true})
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := value.(string); !ok || got != c.want {
			t.Fatalf("pyjson read %q as %q, want %q", c.escape, got, c.want)
		}
	}
	d800, d801 := "\xed\xa0\x80", "\xed\xa0\x81"
	message := "[a](skill:///x/crw-" + d800 + "/SKILL.md) [b](skill:///x/crw-" + d801 + "/SKILL.md)"
	got := MentionedFolders(message)
	if len(got) != 2 {
		t.Fatalf("got %d keys %v, want 2: a lone surrogate collapsed the folder names", len(got), got)
	}
	for _, want := range []string{"crw-" + d800, "crw-" + d801} {
		if !got[want] {
			t.Errorf("missing key %q", want)
		}
	}
}

// spawnInlineLowerJS is String.prototype.toLowerCase: it must keep a lone surrogate's bytes
// while it lowercases around it, and keep the U+0130 and final-sigma rules.
func TestSpawnInlineLowerJSPreservesLoneSurrogates(t *testing.T) {
	wtf8 := "\xed\xa0\x80" // U+D800 as the WTF-8 pyjson keeps for a lone surrogate
	// One byte outside any valid sequence, which pyjson.CodePoint reads as U+DC80. No live
	// consumer feeds a raw byte here (the hook decodes its payload with pyjson); the case pins
	// the byte-preserving rule of the function itself.
	byte80 := "\x80"
	cases := []struct{ in, want string }{
		{"A" + wtf8 + "Z", "a" + wtf8 + "z"},
		{wtf8, wtf8},
		{"A" + byte80 + "Z", "a" + byte80 + "z"},
		{byte80, byte80},
		{"\u0130", "i\u0307"},
		{"\u039f\u03a3", "\u03bf\u03c2"},
		{"\u039f\u03a3\u0391", "\u03bf\u03c3\u03b1"},
		// A surrogate is not a cased letter: the sigma before it stays final, and one with no
		// cased letter before it does not become final.
		{"\u039f\u03a3" + wtf8, "\u03bf\u03c2" + wtf8},
		{"\u03a3" + wtf8, "\u03c3" + wtf8},
	}
	for _, c := range cases {
		if got := spawnInlineLowerJS(c.in); got != c.want {
			t.Errorf("spawnInlineLowerJS(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
