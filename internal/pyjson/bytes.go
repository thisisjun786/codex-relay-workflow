package pyjson

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// DecodeBytes is what json.loads does to bytes before it scans them: json.detect_encoding
// chooses UTF-32 or UTF-16 by a byte order mark, UTF-8 with its mark (utf-8-sig), or else the
// NUL bytes among the first four, and the bytes are decoded with errors="surrogatepass". The
// error is str(UnicodeDecodeError) as CPython words it: the codec, the byte or byte interval
// (from the start of the input for UTF-16 and UTF-32, after the mark for utf-8-sig) and the
// reason. A lone surrogate, which a Go string cannot hold, is U+FFFD: one character, as it is
// one in Python, so the scanner's positions stay Python's.
func DecodeBytes(b []byte) (string, error) { return decodeBytes(b, false) }

// DecodeBytesWTF8 is DecodeBytes with each lone surrogate kept as the three bytes of its
// generalized UTF-8 form (ED A0..BF 80..BF, WTF-8), the form a Go string holds a Python str's
// lone surrogate in, rather than as U+FFFD: the characters json.loads builds its strings from,
// where DecodeBytes gives the characters its scanner counts. It fails exactly where DecodeBytes
// fails.
func DecodeBytesWTF8(b []byte) (string, error) { return decodeBytes(b, true) }

func decodeBytes(b []byte, wtf8 bool) (string, error) {
	switch {
	case bytes.HasPrefix(b, []byte{0, 0, 0xfe, 0xff}):
		return decodeUTF32(b, 4, binary.BigEndian, "utf-32-be", wtf8)
	case bytes.HasPrefix(b, []byte{0xff, 0xfe, 0, 0}):
		return decodeUTF32(b, 4, binary.LittleEndian, "utf-32-le", wtf8)
	case bytes.HasPrefix(b, []byte{0xfe, 0xff}):
		return decodeUTF16(b, 2, binary.BigEndian, "utf-16-be", wtf8)
	case bytes.HasPrefix(b, []byte{0xff, 0xfe}):
		return decodeUTF16(b, 2, binary.LittleEndian, "utf-16-le", wtf8)
	case bytes.HasPrefix(b, []byte{0xef, 0xbb, 0xbf}):
		return decodeSurrogateUTF8(b[3:], wtf8)
	}
	switch {
	case len(b) >= 4 && b[0] == 0 && b[1] != 0:
		return decodeUTF16(b, 0, binary.BigEndian, "utf-16-be", wtf8)
	case len(b) >= 4 && b[0] == 0:
		return decodeUTF32(b, 0, binary.BigEndian, "utf-32-be", wtf8)
	case len(b) >= 4 && b[1] == 0 && (b[2] != 0 || b[3] != 0):
		return decodeUTF16(b, 0, binary.LittleEndian, "utf-16-le", wtf8)
	case len(b) >= 4 && b[1] == 0:
		return decodeUTF32(b, 0, binary.LittleEndian, "utf-32-le", wtf8)
	case len(b) == 2 && b[0] == 0:
		return decodeUTF16(b, 0, binary.BigEndian, "utf-16-be", wtf8)
	case len(b) == 2 && b[1] == 0:
		return decodeUTF16(b, 0, binary.LittleEndian, "utf-16-le", wtf8)
	}
	return decodeSurrogateUTF8(b, wtf8)
}

// decodeSurrogateUTF8 is bytes.decode("utf-8", "surrogatepass"). The bytes it passes as a lone
// surrogate are already that surrogate's WTF-8 form, so with wtf8 set they stand as they are.
func decodeSurrogateUTF8(b []byte, wtf8 bool) (string, error) {
	text, err := decodeUTF8(b, true)
	if err != nil || !wtf8 {
		return text, err
	}
	return string(b), nil
}

// writeSurrogate writes a lone surrogate: U+FFFD, one character as it is one in Python, or with
// wtf8 set its WTF-8 bytes.
func writeSurrogate(s *strings.Builder, unit rune, wtf8 bool) {
	if !wtf8 {
		s.WriteRune(unit) // a lone surrogate is written as U+FFFD
		return
	}
	s.Write([]byte{byte(0xe0 | unit>>12), byte(0x80 | (unit>>6)&0x3f), byte(0x80 | unit&0x3f)})
}

// codecError is str(UnicodeDecodeError) for the bytes b[start:end].
func codecError(codec string, b []byte, start, end int, reason string) error {
	if end-start == 1 {
		return fmt.Errorf("'%s' codec can't decode byte 0x%02x in position %d: %s", codec, b[start], start, reason)
	}
	return fmt.Errorf("'%s' codec can't decode bytes in position %d-%d: %s", codec, start, end-1, reason)
}

// decodeUTF16 decodes b[from:]: a pair of surrogates is one character, a lone one passes as one
// (surrogatepass), and an odd byte at the end is truncated data.
func decodeUTF16(b []byte, from int, order binary.ByteOrder, codec string, wtf8 bool) (string, error) {
	var s strings.Builder
	for i := from; i < len(b); i += 2 {
		if i+1 == len(b) {
			return "", codecError(codec, b, i, i+1, "truncated data")
		}
		unit := rune(order.Uint16(b[i:]))
		if utf16.IsSurrogate(unit) && unit < 0xdc00 && i+3 < len(b) {
			if r := utf16.DecodeRune(unit, rune(order.Uint16(b[i+2:]))); r != utf8.RuneError {
				s.WriteRune(r)
				i += 2
				continue
			}
		}
		if utf16.IsSurrogate(unit) {
			writeSurrogate(&s, unit, wtf8)
			continue
		}
		s.WriteRune(unit)
	}
	return s.String(), nil
}

// decodeUTF32 decodes b[from:]: a surrogate passes as one character, a value past U+10FFFF is
// out of range, and one to three bytes at the end are truncated data.
func decodeUTF32(b []byte, from int, order binary.ByteOrder, codec string, wtf8 bool) (string, error) {
	var s strings.Builder
	for i := from; i < len(b); i += 4 {
		if i+4 > len(b) {
			return "", codecError(codec, b, i, len(b), "truncated data")
		}
		value := order.Uint32(b[i:])
		if value > 0x10ffff {
			return "", codecError(codec, b, i, i+4, "code point not in range(0x110000)")
		}
		if utf16.IsSurrogate(rune(value)) {
			writeSurrogate(&s, rune(value), wtf8)
			continue
		}
		s.WriteRune(rune(value))
	}
	return s.String(), nil
}
