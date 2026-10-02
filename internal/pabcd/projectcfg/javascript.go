package projectcfg

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// This file reads and writes JSON the way JavaScript does, because the oracle round-trips crw.json
// through JSON.parse and JSON.stringify(value, null, 2): members keep their order (integer-index keys
// first), strings keep lone surrogates and U+2028/U+2029, numbers print as ECMAScript prints them.
// Strings are Go strings in WTF-8 (a lone surrogate is the bytes ED A0..BF xx).

type member struct {
	key   string
	value any
}

// object is a JSON object whose members keep their order.
type object []member

func (o object) get(key string) any {
	for _, m := range o {
		if m.key == key {
			return m.value
		}
	}
	return nil
}

// set assigns like a JavaScript property: an existing key keeps its position.
func (o object) set(key string, value any) object {
	for i := range o {
		if o[i].key == key {
			o[i].value = value
			return o
		}
	}
	return append(o, member{key, value})
}

// jsOrder puts the members whose keys are array indices first, ascending, as JavaScript enumerates
// an object's own keys; the others keep their order.
func (o object) jsOrder() object {
	rank := func(m member) uint64 {
		n, err := strconv.ParseUint(m.key, 10, 32)
		if err != nil || n == 1<<32-1 || strconv.FormatUint(n, 10) != m.key {
			return math.MaxUint64
		}
		return n
	}
	slices.SortStableFunc(o, func(a, b member) int { return cmp.Compare(rank(a), rank(b)) })
	return o
}

// parser reads a document with encoding/json for the structure and takes each string literal from
// the input bytes, because the decoder's own string loses a lone surrogate to U+FFFD.
type parser struct {
	data    []byte
	decoder *json.Decoder
	offset  int64 // end of the previous token
	err     error // the first error; after it every read returns nothing
}

// next is the next token and, for a string, its literal: the input since the previous token starts
// with only whitespace, a comma or a colon before the opening quote.
func (p *parser) next() (json.Token, []byte) {
	token, err := p.decoder.Token()
	if p.err == nil {
		p.err = err
	}
	end := p.decoder.InputOffset()
	span := p.data[p.offset:end]
	p.offset = end
	return token, span[max(bytes.IndexByte(span, '"'), 0):]
}

// parse is JSON.parse of one document read from a file; ok is false, and value nil, for anything
// JSON.parse rejects.
func parse(data []byte) (value any, ok bool) {
	data = decodeUTF8(data)
	p := &parser{data: data, decoder: json.NewDecoder(bytes.NewReader(data))}
	p.decoder.UseNumber()
	value = p.value()
	if _, extra := p.decoder.Token(); p.err != nil || !errors.Is(extra, io.EOF) {
		return nil, false
	}
	return value, true
}

// decodeUTF8 reads the bytes as Node's utf8 decoder does: each maximal subpart of an ill-formed
// sequence becomes one U+FFFD. What is left is valid UTF-8, so a raw byte can never pass for the
// WTF-8 bytes of an escaped lone surrogate.
func decodeUTF8(data []byte) []byte {
	var out []byte
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size == 1 {
			size, out = subpart(data[i:]), utf8.AppendRune(out, utf8.RuneError)
		} else {
			out = append(out, data[i:i+size]...)
		}
		i += size
	}
	return out
}

// subpart is the length of the longest prefix of b that starts a well-formed UTF-8 sequence.
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

func (p *parser) value() any {
	token, span := p.next()
	switch t := token.(type) {
	case json.Delim:
		if t == '[' {
			items := []any{}
			for p.err == nil && p.decoder.More() {
				items = append(items, p.value())
			}
			p.next()
			return items
		}
		members := object{}
		for p.err == nil && p.decoder.More() {
			_, key := p.next()
			members = members.set(unquote(key), p.value())
		}
		p.next()
		return members.jsOrder()
	case string:
		return unquote(span)
	case json.Number:
		number, _ := strconv.ParseFloat(string(t), 64)
		switch {
		case math.IsInf(number, 0): // JSON.parse reads 1e999 as Infinity, which JSON.stringify writes as null
			return nil
		case number == 0: // and writes -0 as 0
			return float64(0)
		}
		return number
	}
	return token
}

const escapes, escaped = "bfnrt", "\b\f\n\r\t"

// unquote is the value of a string literal JSON.parse accepted. A lone surrogate escape is kept as
// the WTF-8 bytes of that code unit rather than becoming U+FFFD.
func unquote(literal []byte) string {
	if len(literal) < 2 { // only after a syntax error, which parse reports
		return ""
	}
	s := string(literal[1 : len(literal)-1])
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if k := strings.IndexByte(escapes, s[i]); k >= 0 {
			b.WriteByte(escaped[k])
		} else if s[i] != 'u' { // \" \\ \/
			b.WriteByte(s[i])
		} else if unit, _ := strconv.ParseUint(s[i+1:i+5], 16, 16); unit < 0xD800 || unit >= 0xE000 {
			b.WriteRune(rune(unit))
			i += 4
		} else if low, _ := strconv.ParseUint(s[min(i+7, len(s)):min(i+11, len(s))], 16, 16); unit < 0xDC00 && strings.HasPrefix(s[i+5:], `\u`) && low >= 0xDC00 && low < 0xE000 {
			b.WriteRune(0x10000 + rune(unit-0xD800)<<10 + rune(low-0xDC00))
			i += 10
		} else { // a lone surrogate
			b.Write([]byte{0xED, 0x80 | byte(unit>>6&0x3F), 0x80 | byte(unit&0x3F)})
			i += 4
		}
	}
	return b.String()
}

// quote is JSON.stringify of a string: the quote, backslash and control characters escaped, a lone
// surrogate as \udXXX, and nothing else (U+2028 and U+2029 stay raw).
func quote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch k := strings.IndexRune(escaped, r); {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case k >= 0:
			b.WriteByte('\\')
			b.WriteByte(escapes[k])
		case r < 0x20:
			fmt.Fprintf(&b, `\u%04x`, r)
		case r == utf8.RuneError && size == 1 && s[i] == 0xED && i+2 < len(s) && s[i+1]&0xE0 == 0xA0 && s[i+2]&0xC0 == 0x80:
			fmt.Fprintf(&b, `\u%04x`, 0xD000|rune(s[i+1]&0x3F)<<6|rune(s[i+2]&0x3F))
			size = 3
		default:
			b.WriteString(s[i : i+size])
		}
		i += size
	}
	b.WriteByte('"')
	return b.String()
}

// stringify is JSON.stringify(value, null, 2). Numbers go through encoding/json, which writes a
// float64 as ECMAScript does; the parser has already turned -0 into 0 and an overflow into null.
func stringify(v any, indent string) string {
	inner := indent + "  "
	var open, closing string
	var parts []string
	switch v := v.(type) {
	case object:
		open, closing = "{", "}"
		for _, m := range v {
			parts = append(parts, inner+quote(m.key)+": "+stringify(m.value, inner))
		}
	case []any:
		open, closing = "[", "]"
		for _, item := range v {
			parts = append(parts, inner+stringify(item, inner))
		}
	case string:
		return quote(v)
	case float64:
		number, _ := json.Marshal(v)
		return string(number)
	case bool:
		return strconv.FormatBool(v)
	default:
		return "null"
	}
	if len(parts) == 0 {
		return open + closing
	}
	return open + "\n" + strings.Join(parts, ",\n") + "\n" + indent + closing
}
