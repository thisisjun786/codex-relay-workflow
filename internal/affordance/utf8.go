package affordance

import (
	"strings"
	"unicode/utf8"
)

// nodeUTF8 is Buffer.toString("utf8"): replace each invalid maximal subpart,
// rather than merging adjacent invalid bytes as strings.ToValidUTF8 does.
func nodeUTF8(data []byte) string {
	var b strings.Builder
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size > 1 {
			b.Write(data[i : i+size])
			i += size
			continue
		}
		need, lo, hi := 0, byte(0x80), byte(0xbf)
		switch c := data[i]; {
		case c >= 0xc2 && c <= 0xdf:
			need = 2
		case c >= 0xe0 && c <= 0xef:
			need = 3
			if c == 0xe0 {
				lo = 0xa0
			}
			if c == 0xed {
				hi = 0x9f
			}
		case c >= 0xf0 && c <= 0xf4:
			need = 4
			if c == 0xf0 {
				lo = 0x90
			}
			if c == 0xf4 {
				hi = 0x8f
			}
		}
		used := 1
		if need > 0 && i+1 < len(data) && data[i+1] >= lo && data[i+1] <= hi {
			used = 2
			for used < need && i+used < len(data) && data[i+used] >= 0x80 && data[i+used] <= 0xbf {
				used++
			}
		}
		b.WriteRune(utf8.RuneError)
		i += used
	}
	return b.String()
}
