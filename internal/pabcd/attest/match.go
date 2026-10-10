package attest

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// The JavaScript semantics the oracle leans on. Its regular expressions are written out by hand: Go's regexp folds case over all of
// Unicode (the KELVIN SIGN would match "k") where the oracle's flags fold ASCII only, and no package variable may do work.

// asciiLower lowercases A to Z only, as a JavaScript regular expression without the u flag compares under /i.
func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		return r
	}, s)
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
