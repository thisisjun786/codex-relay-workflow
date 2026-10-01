package pyjson

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DecodeUTF8 is Python's strict UTF-8 text-file decoding, including the byte
// interval and reason in UnicodeDecodeError (before universal newlines).
func DecodeUTF8(data []byte) (string, error) { return decodeUTF8(data, false) }

// decodeUTF8 is bytes.decode("utf-8"), with errors="surrogatepass" when surrogates is set: an
// encoded surrogate (ED A0..BF 80..BF), which the strict decoder refuses at its first byte, is
// then one character, a lone surrogate, which a Go string holds as U+FFFD.
func decodeUTF8(data []byte, surrogates bool) (string, error) {
	var passed []int
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r != utf8.RuneError || size != 1 {
			i += size
			continue
		}
		if surrogates && i+2 < len(data) && data[i] == 0xed && data[i+1] >= 0xa0 && data[i+1] <= 0xbf && data[i+2] >= 0x80 && data[i+2] <= 0xbf {
			passed = append(passed, i)
			i += 3
			continue
		}
		lead := data[i]
		n, reason := illFormed(data, i)
		where := fmt.Sprintf("byte 0x%02x in position %d", lead, i)
		if n > 1 {
			where = fmt.Sprintf("bytes in position %d-%d", i, i+n-1)
		}
		return "", fmt.Errorf("'utf-8' codec can't decode %s: %s", where, reason)
	}
	if len(passed) == 0 {
		return string(data), nil
	}
	var b strings.Builder
	last := 0
	for _, at := range passed {
		b.Write(data[last:at])
		b.WriteRune(utf8.RuneError)
		last = at + 3
	}
	b.Write(data[last:])
	return b.String(), nil
}

// illFormed is the ill-formed sequence starting at data[i] that CPython's UTF-8 decoder reports
// as one error: its length in bytes (the maximal subpart of a well-formed sequence, or the one
// byte that cannot start one) and the reason str(UnicodeDecodeError) gives for it.
func illFormed(data []byte, i int) (int, string) {
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
	return n, reason
}
