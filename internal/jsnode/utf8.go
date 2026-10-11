package jsnode

import "unicode/utf8"

// DecodeUTF8 is Buffer.toString("utf8"): each maximal invalid subpart of b becomes one U+FFFD, as the WHATWG decoder defines it, where
// Go's own conversion emits one per byte. The result is valid UTF-8.
func DecodeUTF8(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r == utf8.RuneError && size == 1 {
			out, size = utf8.AppendRune(out, utf8.RuneError), subpart(b[i:])
		} else {
			out = append(out, b[i:i+size]...)
		}
		i += size
	}
	return string(out)
}

// subpart is the length of the longest prefix of b that starts a well-formed UTF-8 sequence (Unicode table 3-7); a byte that starts none
// is a subpart of its own.
func subpart(b []byte) int {
	lo, hi, need := byte(0x80), byte(0xBF), 0
	switch c := b[0]; {
	case c >= 0xC2 && c <= 0xDF:
		need = 1
	case c == 0xE0:
		need, lo = 2, 0xA0
	case c == 0xED:
		need, hi = 2, 0x9F
	case c >= 0xE1 && c <= 0xEF:
		need = 2
	case c == 0xF0:
		need, lo = 3, 0x90
	case c == 0xF4:
		need, hi = 3, 0x8F
	case c >= 0xF1 && c <= 0xF3:
		need = 3
	}
	n := 1
	for ; n <= need && n < len(b) && b[n] >= lo && b[n] <= hi; n++ {
		lo, hi = 0x80, 0xBF
	}
	return n
}
