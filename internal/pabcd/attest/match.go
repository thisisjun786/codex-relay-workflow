package attest

import (
	"encoding/json"
	"math"
	"strings"
	"unicode"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The JavaScript semantics the oracle leans on. Its regular expressions are written out by hand: Go's regexp folds case over all of
// Unicode (the KELVIN SIGN would match "k") where the oracle's flags fold ASCII only, and no package variable may do work.

// asciiLower lowercases A to Z only, as a JavaScript regular expression without the u flag compares under /i.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

// isPlaceholderDid is /^(tbd|todo|n\/?a|none|done|ok|\.+|-+)$/i of attest.ts:66.
func isPlaceholderDid(s string) bool {
	switch asciiLower(s) {
	case "tbd", "todo", "na", "n/a", "none", "done", "ok":
		return true
	}
	return s != "" && (strings.Trim(s, ".") == "" || strings.Trim(s, "-") == "")
}

// verdictBody is what follows /^verdict\s*[:=]/i on a line, and false when the line does not start so.
func verdictBody(line string) (string, bool) {
	if len(line) < 7 || asciiLower(line[:7]) != "verdict" {
		return "", false
	}
	rest := text.Trim(line[7:])
	if rest == "" || (rest[0] != ':' && rest[0] != '=') {
		return "", false
	}
	return rest[1:], true
}

// HasFailVerdictTail is attest.ts hasFailVerdictTail: true when the last verdict-shaped line of the final five non-empty lines
// of auditOutput reads /^verdict\s*[:=]\s*fail\b/i (an earlier FAIL corrected by a later PASS, or FAIL in prose, does not count).
func HasFailVerdictTail(auditOutput string) bool {
	var lines []string
	for _, l := range text.SplitLines(auditOutput) {
		if l = text.Trim(l); l != "" {
			lines = append(lines, l)
		}
	}
	if len(lines) > 5 {
		lines = lines[len(lines)-5:]
	}
	last, found := "", false
	for _, l := range lines {
		if body, ok := verdictBody(l); ok {
			last, found = body, true
		}
	}
	body := text.Trim(last)
	// \b after the final "l" of fail: the next character is not an ASCII word character (a non-ASCII one is not).
	return found && len(body) >= 4 && asciiLower(body[:4]) == "fail" && (len(body) == 4 || !isWordByte(body[4]))
}

func isWordByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_'
}

// isNumberedDoc is /^\d{3}_.+\.md$/ of plan-gate.ts:31: three ASCII digits, an underscore, at least one character that is not a
// line terminator, and ".md" (case-sensitive).
func isNumberedDoc(name string) bool {
	if len(name) < 8 || name[3] != '_' || !strings.HasSuffix(name, ".md") || strings.ContainsAny(name[4:], "\n\r\u2028\u2029") {
		return false
	}
	for _, c := range []byte(name[:3]) {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// jsNumber is a number in a template literal: encoding/json prints a float64 as ECMAScript does, bar NaN, Infinity and -0.
func jsNumber(f float64) string {
	switch {
	case math.IsNaN(f):
		return "NaN"
	case math.IsInf(f, 1):
		return "Infinity"
	case math.IsInf(f, -1):
		return "-Infinity"
	case f == 0:
		return "0"
	}
	b, _ := json.Marshal(f)
	return string(b)
}

// lowerJS is String.prototype.toLowerCase: the simple case mapping, with U+0130 as "i" and a combining dot above, and capital
// sigma in its final form at the end of a word (Unicode Final_Sigma).
func lowerJS(s string) string {
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

// caseIgnorable is the Unicode Case_Ignorable property: the categories Mn, Me, Cf, Lm and Sk, and the Word_Break classes
// MidLetter, MidNumLet and Single_Quote, which Go's tables do not carry. The data is Go's: a character assigned after the Unicode
// version of Go's tables or of the oracle's runtime may differ.
func caseIgnorable(r rune) bool {
	return unicode.In(r, unicode.Mn, unicode.Me, unicode.Cf, unicode.Lm, unicode.Sk) ||
		strings.ContainsRune("'.:\u00b7\u0387\u055f\u05f4\u2018\u2019\u2024\u2027\ufe13\ufe52\ufe55\uff07\uff0e\uff1a", r)
}
