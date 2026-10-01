package settings

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Repr is Python's repr() of a string: the quote Python picks and its escapes, so a message
// built from it reads byte for byte like the f"{value!r}" it ports. Every character
// str.isprintable() rejects (Cc, Cf, Cs, Co, Cn, Zl, Zp, and Zs other than the space) is
// escaped, as \xNN below U+0100, \uNNNN in the rest of the BMP and \UNNNNNNNN above it. The code
// points are read as pyjson.CodePoint reads them, so a lone surrogate prints as \udXXX, as Python
// prints it, and never as raw bytes or U+FFFD. Printability is CPython 3.14's (Printable), so a
// character assigned in a later Unicode version than that interpreter's is escaped as it is.
func Repr(s string) string {
	quote := "'"
	if strings.Contains(s, "'") && !strings.Contains(s, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for i := 0; i < len(s); {
		r, size := pyjson.CodePoint(s, i)
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
		case Printable(r):
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

// StderrText is text as Python's sys.stderr writes it, which always uses
// errors="backslashreplace": a lone surrogate, held as WTF-8 (pyjson.CodePoint), is written as its
// \uXXXX escape and everything else as it stands. A path that holds one reaches a message
// unescaped, as Python's str() of it does, and only the stream escapes it.
func StderrText(text string) string {
	var b strings.Builder
	for i := 0; i < len(text); {
		r, size := pyjson.CodePoint(text, i)
		if r >= 0xd800 && r <= 0xdfff {
			fmt.Fprintf(&b, "\\u%04x", r)
		} else {
			b.WriteString(text[i : i+size])
		}
		i += size
	}
	return b.String()
}
