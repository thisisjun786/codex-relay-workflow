package jsnode

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Lower is String.prototype.toLowerCase: the simple case mapping, with U+0130 as "i" and a combining dot above, and capital sigma in
// its final form at the end of a word (Unicode Final_Sigma). A lone surrogate is no code point to case-fold: it keeps the bytes it was
// spelled with, whether pyjson.CodePoint read it from WTF-8 or from a surrogateescape byte, as the JS reading leaves it. The data is
// Go's: a character assigned after the Unicode version of Go's tables or of the oracle's runtime may differ.
func Lower(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return strings.ToLower(s)
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		switch {
		case pyjson.IsSurrogate(r):
			b.WriteString(s[i : i+n])
		case r == 0x130:
			b.WriteString("i\u0307")
		case r == 0x3a3 && finalSigma(s, i, n):
			b.WriteRune(0x3c2)
		default:
			b.WriteRune(unicode.ToLower(r))
		}
		i += n
	}
	return b.String()
}

// finalSigma: a cased letter precedes the sigma that spans s[i:i+n] (skipping case-ignorable characters) and none follows it
// (likewise). The text before it is read backwards, which agrees with the forward reading for every class this decides on: a byte of a
// lone surrogate or of invalid UTF-8 reads as a character that is neither cased nor case-ignorable either way.
func finalSigma(s string, i, n int) bool {
	j := i
	for {
		if j == 0 {
			return false
		}
		r, size := utf8.DecodeLastRuneInString(s[:j])
		j -= size
		if caseIgnorable(r) {
			continue
		}
		if !cased(r) {
			return false
		}
		break
	}
	for k := i + n; k < len(s); {
		r, size := pyjson.CodePoint(s, k)
		k += size
		if caseIgnorable(r) {
			continue
		}
		return !cased(r)
	}
	return true
}

// cased is the Unicode Cased property: Lowercase, Uppercase and titlecase letters.
func cased(r rune) bool {
	return unicode.IsUpper(r) || unicode.IsLower(r) || unicode.Is(unicode.Lt, r) || unicode.Is(unicode.Other_Lowercase, r) || unicode.Is(unicode.Other_Uppercase, r)
}

// caseIgnorable is the Unicode Case_Ignorable property: the categories Mn, Me, Cf, Lm and Sk, and the Word_Break classes MidLetter,
// MidNumLet and Single_Quote, which Go's tables do not carry.
func caseIgnorable(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) ||
		strings.ContainsRune("'.:\u00b7\u0387\u055f\u05f4\u2018\u2019\u2024\u2027\ufe13\ufe52\ufe55\uff07\uff0e\uff1a", r)
}
