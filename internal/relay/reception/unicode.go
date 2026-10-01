package reception

import (
	"strconv"
	"unicode/utf16"
)

// CheckJSONText refuses a packet whose JSON text escapes a lone UTF-16 surrogate (a \ud800 to
// \udfff escape without its pair): no reader can hash or render that text, and encoding/json
// would read it as U+FFFD before Check could see it.
func CheckJSONText(raw []byte) error {
	inString := false
	for i := 0; i < len(raw); i++ {
		c := raw[i]
		switch {
		case !inString:
			inString = c == '"'
		case c == '"':
			inString = false
		case c == '\\' && i+1 < len(raw) && raw[i+1] == 'u' && i+6 <= len(raw):
			code, err := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
			if err != nil || !utf16.IsSurrogate(rune(code)) {
				i++
				continue
			}
			if code < 0xdc00 && i+12 <= len(raw) && string(raw[i+6:i+8]) == `\u` {
				if low, err := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16); err == nil && low >= 0xdc00 && low <= 0xdfff {
					i += 11
					continue
				}
			}
			return malformed("the packet holds text that is not valid Unicode (the lone surrogate escape \\u%04x at byte %d), which no reader can hash or render", code, i)
		case c == '\\':
			i++
		}
	}
	return nil
}
