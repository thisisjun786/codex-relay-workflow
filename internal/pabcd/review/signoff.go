package review

import (
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

// ReviewSignoff is the identity and verdict in a reviewer's closing lines.
type ReviewSignoff struct {
	LaunchID string        `json:"launchId"`
	Verdict  ReviewVerdict `json:"verdict"`
	// Blockers and Findings are the count and finding references of a `GO-WITH-FIXES (blockers=N; findings=a,b)` verdict line
	// (CRW-1116, port: fixed); both are empty for the bare words, which stay valid.
	Blockers int      `json:"blockers,omitempty"`
	Findings []string `json:"findings,omitempty"`
}

// ParseSignoff parses only the last two nonempty trimmed lines (TS:372-390).
// Empty input is the Go equivalent of the oracle's null/undefined/non-string input.
func ParseSignoff(message string) *ReviewSignoff {
	lines := []string{}
	for _, line := range text.SplitLines(message) {
		if line = text.Trim(line); line != "" {
			lines = append(lines, line)
		}
	}
	if len(lines) < 2 {
		return nil
	}
	launch, ok := reviewSignoffField(lines[len(lines)-2], "LAUNCH")
	if !ok {
		return nil
	}
	for _, r := range launch {
		if text.Trim(string(r)) == "" {
			return nil
		}
	}
	raw, ok := reviewSignoffField(lines[len(lines)-1], "VERDICT")
	if !ok {
		return nil
	}
	line, ok := parseVerdictLine(raw)
	if !ok {
		return nil
	}
	return &ReviewSignoff{LaunchID: launch, Verdict: line.verdict, Blockers: line.blockers, Findings: line.findings}
}
func reviewSignoffField(line, label string) (string, bool) {
	if len(line) < len(label) {
		return "", false
	}
	for i := range label {
		c := line[i]
		if c >= 'a' && c <= 'z' {
			c -= 32
		}
		if c != label[i] {
			return "", false
		}
	}
	rest := text.Trim(line[len(label):])
	if !strings.HasPrefix(rest, ":") {
		return "", false
	}
	value := text.Trim(rest[1:])
	return value, value != ""
}

// JS toUpperCase expands these non-ASCII characters into ASCII. Other non-ASCII
// runes cannot become one of the four verdict words, so they remain unmatched.
func reviewVerdictUpper(s string) string {
	var out strings.Builder
	for _, r := range s {
		switch r {
		case 'ß':
			out.WriteString("SS")
		case 'ı':
			out.WriteByte('I')
		case 'ſ':
			out.WriteByte('S')
		case 'ﬀ':
			out.WriteString("FF")
		case 'ﬁ':
			out.WriteString("FI")
		case 'ﬂ':
			out.WriteString("FL")
		case 'ﬃ':
			out.WriteString("FFI")
		case 'ﬄ':
			out.WriteString("FFL")
		case 'ﬅ', 'ﬆ':
			out.WriteString("ST")
		default:
			if r >= 'a' && r <= 'z' {
				r -= 32
			}
			out.WriteRune(r)
		}
	}
	return out.String()
}
