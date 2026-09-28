package reception

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// CheckJSONText preserves JSON's unpaired UTF-16 escapes until the packet boundary rejects
// them. encoding/json otherwise silently replaces them with U+FFFD before Check can see them.
// The position is in Python's json.dumps(..., ensure_ascii=False) text, not its UTF-8 bytes.
func CheckJSONText(raw []byte) error {
	var compact strings.Builder
	inString := false
	position := 0
	for i := 0; i < len(raw); {
		ch := raw[i]
		if !inString {
			if ch == ' ' || ch == '\n' || ch == '\r' || ch == '\t' {
				i++
				continue
			}
			compact.WriteByte(ch)
			position++
			i++
			if ch == '"' {
				inString = true
			}
			if ch == ',' || ch == ':' {
				compact.WriteByte(' ')
				position++
			}
			continue
		}
		if ch == '"' {
			inString = false
			compact.WriteByte(ch)
			position++
			i++
			continue
		}
		if ch == '\\' && i+1 < len(raw) {
			if raw[i+1] == 'u' && i+6 <= len(raw) {
				code, e := strconv.ParseUint(string(raw[i+2:i+6]), 16, 16)
				if e != nil {
					return e
				}
				r := rune(code)
				if r >= 0xd800 && r <= 0xdbff && i+12 <= len(raw) && string(raw[i+6:i+8]) == "\\u" {
					low, e := strconv.ParseUint(string(raw[i+8:i+12]), 16, 16)
					if e == nil && low >= 0xdc00 && low <= 0xdfff {
						compact.WriteRune(utf16.DecodeRune(r, rune(low)))
						position++
						i += 12
						continue
					}
				}
				if r >= 0xd800 && r <= 0xdfff {
					return malformed("the packet holds text that is not valid Unicode ('utf-8' codec can't encode character '\\u%04x' in position %d: surrogates not allowed), which no reader can hash or render", r, position)
				}
				compact.WriteRune(r)
				position++
				i += 6
				continue
			}
			compact.Write(raw[i : i+2])
			position += 2
			i += 2
			continue
		}
		_, n := utf8.DecodeRune(raw[i:])
		compact.Write(raw[i : i+n])
		position++
		i += n
	}
	if !json.Valid(raw) {
		return fmt.Errorf("invalid packet JSON")
	}
	return nil
}
