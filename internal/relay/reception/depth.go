package reception

import (
	"strconv"
)

// JSONReaderDepthProblem is why a JSON document whose containers nest deeper than a reader
// follows (9,998 levels) is not read, or ""; JSONSettingsDepthProblem is the same for recorded
// settings, whose bound is one level shallower.
func JSONReaderDepthProblem(raw []byte) string   { return jsonDepthProblem(raw, 9998) }
func JSONSettingsDepthProblem(raw []byte) string { return jsonDepthProblem(raw, 9997) }

func jsonDepthProblem(raw []byte, maximum int) string {
	depth := 0
	quoted, escape := false, false
	for _, c := range raw {
		if quoted {
			if escape {
				escape = false
			} else if c == '\\' {
				escape = true
			} else if c == '"' {
				quoted = false
			}
			continue
		}
		switch c {
		case '"':
			quoted = true
		case '{', '[':
			depth++
			if depth > maximum {
				return "the JSON nests deeper than " + strconv.Itoa(maximum) + " levels"
			}
		case '}', ']':
			depth--
		}
	}
	return ""
}
