package settings

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Repr is Python's repr() of a string: the quote Python picks and its escapes, so a message
// built from it reads byte for byte like the f"{value!r}" it ports. Every character
// str.isprintable() rejects (Cc, Cf, Cs, Co, Cn, Zl, Zp, and Zs other than the space) is
// escaped, as \xNN below U+0100, \uNNNN in the rest of the BMP and \UNNNNNNNN above it. The code
// points are read as CodePoint reads them, so a lone surrogate prints as \udXXX, as Python
// prints it, and never as raw bytes or U+FFFD. Printability comes from Go's Unicode tables, so
// a character assigned in a later Unicode version than the Python interpreter's prints raw here
// where that interpreter escapes it as unassigned.
func Repr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for i := 0; i < len(s); {
		r, size := CodePoint(s, i)
		i += size
		switch {
		case r == '\\':
			b.WriteString(`\\`)
		case string(r) == quote:
			b.WriteString(`\` + quote)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case unicode.IsPrint(r) && !isSurrogate(r):
			b.WriteString(s[i-size : i])
		case r < 0x100:
			fmt.Fprintf(&b, `\x%02x`, r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			fmt.Fprintf(&b, `\U%08x`, r)
		}
	}
	b.WriteString(quote)
	return b.String()
}

// CodePoint is the Python str code point a Go string holds at s[i], and how many bytes it takes.
// Two spellings reach a Go string for a code point UTF-8 cannot carry: a lone surrogate a JSON
// decoder kept as its three-byte generalized UTF-8 form (ED A0..BF 80..BF, WTF-8), and a byte
// outside any valid sequence, which a path or argv holds where Python's surrogateescape decoding
// holds U+DC00 plus that byte. Both are read as the surrogate. A raw path that happens to hold
// the three WTF-8 bytes of a surrogate reads as that one surrogate, where Python would read three
// escaped bytes: a Go string does not record which of the two it came from.
func CodePoint(s string, i int) (rune, int) {
	if i+2 < len(s) && s[i] == 0xed && s[i+1] >= 0xa0 && s[i+1] <= 0xbf && s[i+2] >= 0x80 && s[i+2] <= 0xbf {
		return 0xd000 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f), 3
	}
	r, size := utf8.DecodeRuneInString(s[i:])
	if r == utf8.RuneError && size == 1 {
		return 0xdc00 + rune(s[i]), 1
	}
	return r, size
}

func isSurrogate(r rune) bool { return r >= 0xd800 && r <= 0xdfff }
