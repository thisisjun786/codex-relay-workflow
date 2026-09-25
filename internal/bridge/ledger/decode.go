package ledger

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// DecodeJSON parses JSON the way Python json.loads does for fingerprinting: numbers stay
// json.Number, and a lone UTF-16 surrogate escape such as "\ud800" is kept as that code
// point (stored WTF-8 style) instead of being replaced by U+FFFD as encoding/json does.
// Fingerprint writes such a code point back as the same escape, so a request carrying one
// fingerprints exactly as Python's json.dumps would.
func DecodeJSON(raw []byte) (any, error) {
	p := &parser{src: string(raw)}
	p.space()
	value, err := p.value()
	if err != nil {
		return nil, err
	}
	p.space()
	if p.pos != len(p.src) {
		return nil, p.fail("extra data")
	}
	return value, nil
}

type parser struct {
	src string
	pos int
}

func (p *parser) fail(what string) error {
	return fmt.Errorf("json: %s at offset %d", what, p.pos)
}

func (p *parser) space() {
	for p.pos < len(p.src) && strings.IndexByte(" \t\r\n", p.src[p.pos]) >= 0 {
		p.pos++
	}
}

func (p *parser) literal(word string, value any) (any, error) {
	if !strings.HasPrefix(p.src[p.pos:], word) {
		return nil, p.fail("invalid literal")
	}
	p.pos += len(word)
	return value, nil
}

func (p *parser) value() (any, error) {
	if p.pos >= len(p.src) {
		return nil, p.fail("unexpected end")
	}
	switch c := p.src[p.pos]; {
	case c == '{':
		return p.object()
	case c == '[':
		return p.array()
	case c == '"':
		return p.str()
	case c == 't':
		return p.literal("true", true)
	case c == 'f':
		return p.literal("false", false)
	case c == 'n':
		return p.literal("null", nil)
	default:
		return p.number()
	}
}

func (p *parser) object() (any, error) {
	out := map[string]any{}
	p.pos++
	p.space()
	if p.pos < len(p.src) && p.src[p.pos] == '}' {
		p.pos++
		return out, nil
	}
	for {
		p.space()
		if p.pos >= len(p.src) || p.src[p.pos] != '"' {
			return nil, p.fail("expected object key")
		}
		key, err := p.str()
		if err != nil {
			return nil, err
		}
		p.space()
		if p.pos >= len(p.src) || p.src[p.pos] != ':' {
			return nil, p.fail("expected ':'")
		}
		p.pos++
		p.space()
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		out[key] = item // Python keeps the last duplicate, and so does this.
		if done, err := p.next('}'); done || err != nil {
			return out, err
		}
	}
}

func (p *parser) array() (any, error) {
	out := []any{}
	p.pos++
	p.space()
	if p.pos < len(p.src) && p.src[p.pos] == ']' {
		p.pos++
		return out, nil
	}
	for {
		p.space()
		item, err := p.value()
		if err != nil {
			return nil, err
		}
		out = append(out, item)
		if done, err := p.next(']'); done || err != nil {
			return out, err
		}
	}
}

// next consumes a ',' (not done) or the closing delimiter (done).
func (p *parser) next(closing byte) (bool, error) {
	p.space()
	if p.pos >= len(p.src) {
		return false, p.fail("unexpected end")
	}
	switch p.src[p.pos] {
	case ',':
		p.pos++
		return false, nil
	case closing:
		p.pos++
		return true, nil
	}
	return false, p.fail("expected ',' or closing delimiter")
}

func (p *parser) number() (any, error) {
	start := p.pos
	for p.pos < len(p.src) && strings.IndexByte("+-0123456789.eE", p.src[p.pos]) >= 0 {
		p.pos++
	}
	n := json.Number(p.src[start:p.pos])
	if !json.Valid([]byte(n)) {
		return nil, p.fail("invalid number")
	}
	return n, nil
}

func (p *parser) str() (string, error) {
	var b strings.Builder
	p.pos++
	for {
		if p.pos >= len(p.src) {
			return "", p.fail("unterminated string")
		}
		c := p.src[p.pos]
		switch {
		case c == '"':
			p.pos++
			return b.String(), nil
		case c < 0x20:
			return "", p.fail("control character in string")
		case c != '\\':
			r, size := utf8.DecodeRuneInString(p.src[p.pos:])
			b.WriteRune(r)
			p.pos += size
			continue
		}
		if p.pos+1 >= len(p.src) {
			return "", p.fail("unterminated escape")
		}
		escape := p.src[p.pos+1]
		p.pos += 2
		if simple := strings.IndexByte(`"\/bfnrt`, escape); simple >= 0 {
			b.WriteByte("\"\\/\b\f\n\r\t"[simple])
			continue
		}
		if escape != 'u' {
			return "", p.fail("invalid escape")
		}
		unit, err := p.hex()
		if err != nil {
			return "", err
		}
		if utf16.IsSurrogate(unit) && unit < 0xdc00 && strings.HasPrefix(p.src[p.pos:], `\u`) {
			mark := p.pos
			p.pos += 2
			low, err := p.hex()
			if err == nil && low >= 0xdc00 && low <= 0xdfff {
				b.WriteRune(utf16.DecodeRune(unit, low))
				continue
			}
			p.pos = mark
		}
		writeCodePoint(&b, unit)
	}
}

func (p *parser) hex() (rune, error) {
	if p.pos+4 > len(p.src) {
		return 0, p.fail("short unicode escape")
	}
	value, err := strconv.ParseUint(p.src[p.pos:p.pos+4], 16, 16)
	if err != nil {
		return 0, p.fail("invalid unicode escape")
	}
	p.pos += 4
	return rune(value), nil
}

// writeCodePoint writes any code point up to U+FFFF, including a surrogate that UTF-8
// cannot represent, as its three-byte generalized UTF-8 form.
func writeCodePoint(b *strings.Builder, r rune) {
	if !utf16.IsSurrogate(r) {
		b.WriteRune(r)
		return
	}
	b.WriteByte(byte(0xe0 | r>>12))
	b.WriteByte(byte(0x80 | (r>>6)&0x3f))
	b.WriteByte(byte(0x80 | r&0x3f))
}

// surrogateAt reports the lone surrogate stored at s[i:] by writeCodePoint, if any.
func surrogateAt(s string, i int) (rune, bool) {
	if i+3 > len(s) || s[i] != 0xed || s[i+1] < 0xa0 || s[i+1] > 0xbf || s[i+2] < 0x80 || s[i+2] > 0xbf {
		return 0, false
	}
	return rune(s[i]&0x0f)<<12 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f), true
}
