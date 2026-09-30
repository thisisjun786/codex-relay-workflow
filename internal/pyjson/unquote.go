package pyjson

import (
	"errors"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Unquote is the str json.loads decodes from one string token, quotes included, that the scanner
// has already accepted. An escaped surrogate pair is the code point it encodes, and a lone
// surrogate escape ("\udcff") is kept as that code point, which a Go string holds as WTF-8 (the
// three bytes UTF-8 would give it) where encoding/json would read U+FFFD. settings.Repr and
// settings.CodePoint read it back as the one code point it is.
func Unquote(token string) (string, error) {
	if len(token) < 2 || token[0] != '"' || token[len(token)-1] != '"' {
		return "", errors.New("not a JSON string token")
	}
	body := token[1 : len(token)-1]
	var b strings.Builder
	for i := 0; i < len(body); {
		c := body[i]
		if c != '\\' {
			b.WriteByte(c)
			i++
			continue
		}
		if i+1 >= len(body) {
			return "", errors.New("unterminated escape")
		}
		if simple := strings.IndexByte(`"\/bfnrt`, body[i+1]); simple >= 0 {
			b.WriteByte("\"\\/\b\f\n\r\t"[simple])
			i += 2
			continue
		}
		if body[i+1] != 'u' || i+6 > len(body) {
			return "", errors.New("invalid escape")
		}
		unit, err := strconv.ParseUint(body[i+2:i+6], 16, 16)
		if err != nil {
			return "", err
		}
		i += 6
		r := rune(unit)
		if r >= 0xd800 && r < 0xdc00 && i+6 <= len(body) && body[i] == '\\' && body[i+1] == 'u' {
			if low, err := strconv.ParseUint(body[i+2:i+6], 16, 16); err == nil && low >= 0xdc00 && low <= 0xdfff {
				b.WriteRune(utf16.DecodeRune(r, rune(low)))
				i += 6
				continue
			}
		}
		if r >= 0xd800 && r <= 0xdfff {
			b.WriteByte(byte(0xe0 | r>>12))
			b.WriteByte(byte(0x80 | r>>6&0x3f))
			b.WriteByte(byte(0x80 | r&0x3f))
			continue
		}
		b.WriteRune(r)
	}
	return b.String(), nil
}
