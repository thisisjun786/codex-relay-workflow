package store

import (
	"fmt"
	"unicode/utf8"
)

// DecodeUTF8 is Python's strict UTF-8 text-file decoding, including the byte
// interval and reason in UnicodeDecodeError (before universal newlines).
func DecodeUTF8(data []byte) (string, error) {
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size != 1 {
			i += size
			continue
		}
		lead := data[i]
		need, low, high := 0, byte(0x80), byte(0xbf)
		switch {
		case lead >= 0xc2 && lead <= 0xdf:
			need = 2
		case lead == 0xe0:
			need, low = 3, 0xa0
		case lead == 0xed:
			need, high = 3, 0x9f
		case lead >= 0xe1 && lead <= 0xef:
			need = 3
		case lead == 0xf0:
			need, low = 4, 0x90
		case lead == 0xf4:
			need, high = 4, 0x8f
		case lead >= 0xf1 && lead <= 0xf3:
			need = 4
		}
		n, reason := 1, "invalid start byte"
		if need > 0 {
			reason = "invalid continuation byte"
			for n < need {
				if i+n >= len(data) {
					reason = "unexpected end of data"
					break
				}
				if data[i+n] < low || data[i+n] > high {
					break
				}
				n++
				low, high = 0x80, 0xbf
			}
		}
		where := fmt.Sprintf("byte 0x%02x in position %d", lead, i)
		if n > 1 {
			where = fmt.Sprintf("bytes in position %d-%d", i, i+n-1)
		}
		return "", fmt.Errorf("'utf-8' codec can't decode %s: %s", where, reason)
	}
	return string(data), nil
}
