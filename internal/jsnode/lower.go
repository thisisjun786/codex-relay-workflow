package jsnode

import (
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Lower is String.prototype.toLowerCase: the simple case mapping, with U+0130 as "i" and a combining dot above, and capital sigma in
// its final form at the end of a word (Unicode Final_Sigma). A lone surrogate is no code point to case-fold: it keeps the bytes it was
// spelled with, whether pyjson.CodePoint read it from WTF-8 or from a surrogateescape byte, as the JS reading leaves it. The data is
// Go's: a character assigned after the Unicode version of Go's tables or of the oracle's runtime may differ.
func Lower(s string) string {
	type unit struct {
		r         rune
		off, size int
	}
	units := make([]unit, 0, len(s))
	rs := make([]rune, 0, len(s))
	for i := 0; i < len(s); {
		r, n := pyjson.CodePoint(s, i)
		units, rs = append(units, unit{r, i, n}), append(rs, r)
		i += n
	}
	var b strings.Builder
	for i, u := range units {
		switch {
		case pyjson.IsSurrogate(u.r):
			b.WriteString(s[u.off : u.off+u.size])
		case u.r == 0x130:
			b.WriteString("i\u0307")
		case u.r == 0x3a3 && finalSigma(rs, i):
			b.WriteRune(0x3c2)
		default:
			b.WriteRune(unicode.ToLower(u.r))
		}
	}
	return b.String()
}

// finalSigma: a cased letter precedes rs[i] (skipping case-ignorable characters) and none follows it (likewise).
func finalSigma(rs []rune, i int) bool {
	j := i - 1
	for j >= 0 && caseIgnorable(rs[j]) {
		j--
	}
	if j < 0 || !cased(rs[j]) {
		return false
	}
	k := i + 1
	for k < len(rs) && caseIgnorable(rs[k]) {
		k++
	}
	return k == len(rs) || !cased(rs[k])
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
