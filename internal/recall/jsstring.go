package recall

import (
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// Lower is String.prototype.toLowerCase, copied from internal/pabcd/attest (where it is unexported): the simple case mapping, with
// U+0130 as "i" and a combining dot above, and capital sigma in its final form at the end of a word (Unicode Final_Sigma). Search
// code lowers the text it matches with this function, so that a term and its haystack fold alike.
func Lower(s string) string {
	rs := []rune(s)
	var b strings.Builder
	for i, r := range rs {
		switch {
		case r == 0x130:
			b.WriteString("i\u0307")
		case r == 0x3a3 && finalSigma(rs, i):
			b.WriteRune(0x3c2)
		default:
			b.WriteRune(unicode.ToLower(r))
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

// isJSSpace is the white space of JavaScript's \s: text.Trim strips exactly that set, so a lone character is space iff it trims away.
func isJSSpace(r rune) bool { return text.Trim(string(r)) == "" }
