package attest

import "testing"

// A lone surrogate reaches a Go string as the three WTF-8 bytes pyjson keeps for it (the bytes the
// JSON escape \ud800 decodes to). lowerJS is String.prototype.toLowerCase, which leaves a lone
// surrogate as it is; Go's []rune over those bytes decodes each one as U+FFFD, so the oracle's rule
// is the original bytes, not a replacement. ASCII, U+0130 and the final-sigma rules are unchanged.
func TestLowerJSKeepsALoneSurrogate(t *testing.T) {
	wtf8 := "\xed\xa0\x80" // U+D800 as WTF-8, the spelling pyjson.CodePoint reads back
	for _, tc := range []struct {
		name, in, want string
	}{
		{"lone_surrogate_alone", wtf8, wtf8},
		{"lone_surrogate_between_ascii", "x" + wtf8 + "Y", "x" + wtf8 + "y"},
		{"two_lone_surrogates", wtf8 + wtf8, wtf8 + wtf8},
		{"ascii", "ABC", "abc"},
		{"u0130", "\u0130", "i\u0307"},
		{"final_sigma", "\u039f\u03a3", "\u03bf\u03c2"},
		{"non_final_sigma", "\u03a3\u0391", "\u03c3\u03b1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := lowerJS(tc.in); got != tc.want {
				t.Errorf("lowerJS(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
