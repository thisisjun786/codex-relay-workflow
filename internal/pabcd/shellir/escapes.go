package shellir

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// unescapeUnquoted rewrites the unquoted literal parts of x that hold a
// backslash as single-quoted parts, after removing each backslash escape.
// expand leaves unquoted backslashes in place, so the removal happens here.
func unescapeUnquoted(x *syntax.Word) *syntax.Word {
	parts := make([]syntax.WordPart, len(x.Parts))
	for i, p := range x.Parts {
		if lit, ok := p.(*syntax.Lit); ok && strings.Contains(lit.Value, "\\") {
			parts[i] = &syntax.SglQuoted{Value: unescapeLit(lit.Value)}
			continue
		}
		parts[i] = p
	}
	return &syntax.Word{Parts: parts}
}

func unescapeLit(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) {
			if s[i+1] != '\n' {
				b.WriteByte(s[i+1])
			}
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}

// stripLeadingTabs removes the tabs that begin each line of a <<- body.
func stripLeadingTabs(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimLeft(l, "\t")
	}
	return strings.Join(lines, "\n")
}

// continuationNearComment reports a backslash-newline in a text that also holds a #. bash ends a comment
// at a newline even after a backslash, while the parser reads the backslash-newline as a continuation.
func continuationNearComment(src string) bool {
	return strings.Contains(src, "\\\n") && strings.Contains(src, "#")
}
