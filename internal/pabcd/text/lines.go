// Package text holds the newline idiom of the PABCD ports and the JavaScript-compatible trim. It
// is the Go form of CXC v0.2.40 cxc-ops/src/text-lines.ts.
package text

import "strings"

// EOL is a line ending.
type EOL string

// The two line endings DominantEOL chooses between.
const (
	LF   EOL = "\n"
	CRLF EOL = "\r\n"
)

// SplitLines splits for reading (text.split(/\r?\n/)): CRLF and LF both end a line, and a lone CR
// stays in the line.
func SplitLines(text string) []string {
	lines := strings.Split(text, string(LF))
	for i := 0; i < len(lines)-1; i++ {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	return lines
}

// SplitLinesByteExact splits on LF only, keeping the CR, for callers that record byte offsets.
func SplitLinesByteExact(text string) []string { return strings.Split(text, string(LF)) }

// DominantEOL is CRLF only when it outnumbers the lone LFs.
func DominantEOL(text string) EOL {
	crlf := strings.Count(text, string(CRLF))
	if crlf > strings.Count(text, string(LF))-crlf {
		return CRLF
	}
	return LF
}

// WithEOL rewrites text with eol and keeps whether it ended with a newline.
func WithEOL(text string, eol EOL) string {
	normalized := strings.ReplaceAll(text, string(CRLF), string(LF))
	if eol == LF {
		return normalized
	}
	return strings.ReplaceAll(normalized, string(LF), string(CRLF))
}

// Trim is JavaScript's String.prototype.trim. Unlike strings.TrimSpace it strips U+FEFF and keeps
// U+0085, so a value the oracle compares after trim() is compared the same way here.
func Trim(s string) string { return strings.TrimFunc(s, isJSSpace) }

func isJSSpace(r rune) bool {
	switch r {
	case '\t', '\n', '\v', '\f', '\r', ' ', 0xA0, 0x1680, 0x2028, 0x2029, 0x202F, 0x205F, 0x3000, 0xFEFF:
		return true
	}
	return r >= 0x2000 && r <= 0x200A
}
