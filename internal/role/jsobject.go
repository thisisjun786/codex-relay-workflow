package role

import (
	"bytes"
	"encoding/json"
	"slices"
	"strings"
	"unicode/utf8"
)

// sentinel makes an error a constant: this package has no package-level variable.
type sentinel string

func (e sentinel) Error() string { return string(e) }

const errNotObject = sentinel("not an object")

// member's value is the json.RawMessage read from the file, an *object, or a value to encode. A key that decoded to a string with
// U+FFFD (a lone surrogate escape, which Go cannot hold) is its literal text behind the byte 0xFF, which no decoded key holds, so two
// such keys stay two members and are written back as they were.
type member struct {
	key   string
	value any
}

// object is a JSON object whose members keep their order; assigning an existing key keeps its place, so a repeated key ends as its
// last value at its first position, as JSON.parse reads it.
type object []member

func (o object) index(key string) int {
	for i := range o {
		if o[i].key == key {
			return i
		}
	}
	return -1
}

func (o *object) set(key string, value any) {
	if i := o.index(key); i >= 0 {
		(*o)[i].value = value
		return
	}
	*o = append(*o, member{key, value})
}

func (o *object) remove(key string) bool {
	i := o.index(key)
	if i >= 0 {
		*o = slices.Delete(*o, i, i+1)
	}
	return i >= 0
}

// raw is the bytes of a member as the file wrote it.
func (o object) raw(key string) (json.RawMessage, bool) {
	if i := o.index(key); i >= 0 {
		raw, ok := o[i].value.(json.RawMessage)
		return raw, ok
	}
	return nil, false
}

// MarshalJSON writes the members in order, through Stringify at every layer (json.Marshal's HTML escaping cannot be undone later).
func (o object) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, m := range o {
		if i > 0 {
			b.WriteByte(',')
		}
		lit, isLit := strings.CutPrefix(m.key, "\xff")
		key := []byte(lit)
		if !isLit {
			var err error
			if key, err = Stringify(m.key, ""); err != nil {
				return nil, err
			}
		}
		value, err := Stringify(m.value, "")
		if err != nil {
			return nil, err
		}
		b.Write(key)
		b.WriteByte(':')
		b.Write(value)
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

// parseObject reads one JSON object, keeping each member's bytes. The whole document is validated first, so trailing data, empty
// input, a byte order mark and nesting past 10,000 levels (I9) are refused.
func parseObject(data []byte) (object, error) {
	var whole json.RawMessage
	if err := json.Unmarshal(data, &whole); err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(whole))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return nil, errNotObject
	}
	var o object
	for dec.More() {
		start := dec.InputOffset()
		key, err := dec.Token()
		if err != nil {
			return nil, err
		}
		name := key.(string)
		if strings.ContainsRune(name, utf8.RuneError) {
			name = "\xff" + string(bytes.TrimLeft(whole[start:dec.InputOffset()], " \t\r\n,"))
		}
		var value json.RawMessage
		if err := dec.Decode(&value); err != nil {
			return nil, err
		}
		o.set(name, value)
	}
	return o, nil
}

// Stringify is JSON.stringify(v, null, indent), compact for an empty indent: HTML is not escaped and U+2028 and U+2029 are written
// literally, as internal/pabcd/state encodes its files.
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
