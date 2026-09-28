package reception

// JSONDepthProblem mirrors the Python reader's recursion refusal at the JSON boundary.
// Legitimate settings are bounded separately only where a value is copied into an answer.
func JSONDepthProblem(raw []byte) string { return jsonDepthProblem(raw, 9998) }

// The installed console script reaches the same CPython decoder boundary at every relay read.
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
				kind := "array"
				if c == '{' {
					kind = "object"
				}
				return "maximum recursion depth exceeded while decoding a JSON " + kind + " from a unicode string"
			}
		case '}', ']':
			depth--
		}
	}
	return ""
}
