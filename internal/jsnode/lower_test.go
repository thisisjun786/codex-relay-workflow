package jsnode

import "testing"

// A lone surrogate reaches a Go string as the three WTF-8 bytes pyjson keeps for it, or as a byte outside UTF-8 (a path or argv). The
// oracle's toLowerCase leaves a lone surrogate as it is, so the original bytes stay, not a replacement character.
func TestLowerIsToLowerCase(t *testing.T) {
	wtf8 := "\xed\xa0\x80" // U+D800 as WTF-8, the spelling pyjson.CodePoint reads back
	for _, c := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"ascii", "ABC", "abc"},
		{"ci", "CI", "ci"},
		{"u0130", "\u0130", "i\u0307"},
		{"u0130 word", "\u0130STANBUL", "i\u0307stanbul"},
		{"final sigma", "\u039f\u03a3", "\u03bf\u03c2"},
		{"final sigma ending a word", "\u0391\u03a3 \u03a3", "\u03b1\u03c2 \u03c3"},
		{"non-final sigma", "\u03a3\u0391", "\u03c3\u03b1"},
		{"sigma after a case-ignorable", "A.\u03a3", "a.\u03c2"},
		{"sigma before a cased letter", "A\u03a3B", "a\u03c3b"},
		{"kelvin sign", "\u212a", "k"},
		{"accent", "\u00c9a", "\u00e9a"},
		{"titlecase", "\u01c5", "\u01c6"},
		{"hangul", "\ubc30\ud3ec", "\ubc30\ud3ec"},
		{"lone surrogate alone", wtf8, wtf8},
		{"lone surrogate between ascii", "x" + wtf8 + "Y", "x" + wtf8 + "y"},
		{"two lone surrogates", wtf8 + wtf8, wtf8 + wtf8},
		{"byte outside utf-8", "A\xffB", "a\xffb"},
		{"truncated sequence", "\xedAB", "\xedab"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Lower(c.in); got != c.want {
				t.Errorf("Lower(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
