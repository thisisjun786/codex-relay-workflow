package hook

import (
	"bytes"
	"math"
)

// jsonConstants adapts the validated Python JSON language to encoding/json's
// token stream. Offset-tagged zeros cannot collide with strings or real zeros.
// Keep this local to the hook's loaders; other domains have their own contracts.
func jsonConstants(raw []byte) ([]byte, map[int64]float64) {
	out := make([]byte, 0, len(raw))
	constants := map[int64]float64{}
	quoted, escaped := false, false
	for i := 0; i < len(raw); {
		c := raw[i]
		if !quoted {
			matched := false
			for _, token := range []struct {
				spelling string
				value    float64
			}{
				{"NaN", math.NaN()}, {"Infinity", math.Inf(1)}, {"-Infinity", math.Inf(-1)},
			} {
				if bytes.HasPrefix(raw[i:], []byte(token.spelling)) {
					out = append(out, '0')
					constants[int64(len(out))] = token.value
					i += len(token.spelling)
					matched = true
					break
				}
			}
			if matched {
				continue
			}
		}
		out = append(out, c)
		i++
		if escaped {
			escaped = false
			continue
		}
		if quoted && c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			quoted = !quoted
		}
	}
	return out, constants
}
