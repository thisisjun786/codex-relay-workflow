package jsnode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// Stringify is JSON.stringify(v, null, indent), compact for an empty indent, for any value encoding/json can carry: HTML is not
// escaped and U+2028 and U+2029 are written literally, and there is no trailing newline. Object members print in the order the value
// gives them (a struct's field order, an ordered type's MarshalJSON; a map sorts its keys, which is not JavaScript's order, so a caller
// that needs insertion order passes an ordered type). A float64 prints as encoding/json does, which is ECMAScript's spelling; NaN and
// the infinities are an error here, where JSON.stringify prints null, so a caller that can hold them normalises first.
func Stringify(v any, indent string) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", indent)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	in, out := bytes.TrimSuffix(b.Bytes(), []byte("\n")), make([]byte, 0, b.Len())
	// encoding/json always escapes U+2028 and U+2029, JSON.stringify never does. A backslash and the byte after it are copied together,
	// so an escaped backslash before "u2028" stays text.
	for i := 0; i < len(in); i++ {
		switch {
		case in[i] != '\\':
			out = append(out, in[i])
		case bytes.HasPrefix(in[i:], []byte(`\u2028`)):
			out, i = append(out, "\u2028"...), i+5
		case bytes.HasPrefix(in[i:], []byte(`\u2029`)):
			out, i = append(out, "\u2029"...), i+5
		default:
			out, i = append(out, in[i], in[i+1]), i+1
		}
	}
	return out, nil
}

// JSString is JSON.stringify of a string: the quote, the backslash and the control characters are escaped (\b \f \n \r \t, else
// \u00xx), and every other character, U+2028, U+2029 and markup included, stands as it is. A lone surrogate, which a Go string holds as
// its three WTF-8 bytes (ED A0..BF 80..BF), is written as its lowercase \udXXX escape, as well-formed JSON.stringify does; any other
// byte that is not UTF-8 becomes U+FFFD, as ranging over the string reads it.
func JSString(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r == utf8.RuneError && size == 1 && s[i] == 0xED && i+2 < len(s) && s[i+1]&0xE0 == 0xA0 && s[i+2]&0xC0 == 0x80:
			fmt.Fprintf(&b, `\u%04x`, 0xD000|rune(s[i+1]&0x3F)<<6|rune(s[i+2]&0x3F))
			size = 3
		default:
			b.WriteRune(r)
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}
