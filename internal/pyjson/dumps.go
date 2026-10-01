package pyjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

// Options are json.dumps' keyword arguments, and the three ways a stored spelling of the port
// departs from them. The zero value is json.dumps(value).
type Options struct {
	// Indent is json.dumps(indent=Indent): each member of a non-empty container on its own line,
	// Indent spaces deeper per level, with the separators (",", ": "). Zero is indent=None: one
	// line.
	Indent int
	// Compact is separators=(",", ":").
	Compact bool
	// SortKeys is sort_keys=True: an Object's fields in the byte order of their keys, stably. A
	// map[string]any is always written in that order, having no other.
	SortKeys bool
	// Unicode is ensure_ascii=False: only the quote, the backslash and the controls are escaped.
	Unicode bool

	// Normalize writes a json.Number as json.dumps writes what json.loads made of it: an integer
	// spelling as that int (-0 as 0), a fraction or an exponent as the float's repr (a spelling
	// past float64's range as Infinity). Without it a json.Number is written as it is spelled.
	Normalize bool
	// Bytes is how a string's bytes that are not UTF-8 are written.
	Bytes Bytes
	// Marshal writes every value but a map[string]any, an []any and a []string as encoding/json's
	// json.Marshal writes it; with UnescapeHTML its \u003c, \u003e and \u0026 are then put back
	// as <, > and &, wherever those six characters stand in its output.
	Marshal, UnescapeHTML bool
}

// Bytes is how a writer reads a string's bytes that are not UTF-8.
type Bytes uint8

const (
	// SurrogateEscapes reads a WTF-8 surrogate as its code point and any other byte that is not
	// UTF-8 as its surrogate escape (CodePoint): each is written as its \u escape, as json.dumps
	// writes the str Python holds for them.
	SurrogateEscapes Bytes = iota
	// ReplacedBytes reads a WTF-8 surrogate as its code point and any other byte that is not
	// UTF-8 as U+FFFD, as utf8.DecodeRuneInString reads it (the bridge ledger's fingerprint).
	ReplacedBytes
	// ReplacedAll reads every byte that is not UTF-8, each byte of a WTF-8 surrogate included,
	// as U+FFFD, as Go's range over a string reads it (the fault ledger's ids and delivery's
	// criteria digest).
	ReplacedAll
)

// Dumps is json.dumps(value, **o). An Object, a map[string]any, an []any, a []string, a
// []map[string]any, a string, a bool, nil, an int, an int64, a *big.Int, a float64 (NaN and the
// infinities as NaN, Infinity and -Infinity) and a json.Number are JSON; any other value is
// written as fmt's %v spells it.
func Dumps(value any, o Options) string {
	var b bytes.Buffer
	w := writer{b: &b, o: o, lenient: true}
	_ = w.value(value, 0)
	return b.String()
}

// Encode is Dumps for a caller that must not write a value JSON has no spelling for: any other
// Go value, or a json.Number that is not a JSON number, is an error.
func Encode(value any, o Options) ([]byte, error) {
	var b bytes.Buffer
	w := writer{b: &b, o: o}
	if err := w.value(value, 0); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

type writer struct {
	b       *bytes.Buffer
	o       Options
	lenient bool
}

// separators are json.dumps' item and key separators for these options.
func (w writer) separators() (string, string) {
	item, key := ", ", ": "
	if w.o.Indent > 0 {
		item = ","
	}
	if w.o.Compact {
		item, key = ",", ":"
	}
	return item, key
}

// member writes what comes before member i of a container at depth: the item separator and,
// with an indent, the line break and the member's indentation.
func (w writer) member(i, depth int) {
	if i > 0 {
		item, _ := w.separators()
		w.b.WriteString(item)
	}
	if w.o.Indent > 0 {
		w.b.WriteByte('\n')
		w.b.WriteString(strings.Repeat(" ", w.o.Indent*(depth+1)))
	}
}

// close writes the line break and indentation before a non-empty container's closing bracket.
func (w writer) close(depth int) {
	if w.o.Indent > 0 {
		w.b.WriteByte('\n')
		w.b.WriteString(strings.Repeat(" ", w.o.Indent*depth))
	}
}

func (w writer) object(fields Object, depth int) error {
	if w.o.SortKeys {
		fields = slices.Clone(fields)
		slices.SortStableFunc(fields, func(x, y Field) int { return strings.Compare(x.Key, y.Key) })
	}
	_, key := w.separators()
	w.b.WriteByte('{')
	for i, field := range fields {
		w.member(i, depth)
		if err := w.key(field.Key); err != nil {
			return err
		}
		w.b.WriteString(key)
		if err := w.value(field.Value, depth+1); err != nil {
			return err
		}
	}
	if len(fields) > 0 {
		w.close(depth)
	}
	w.b.WriteByte('}')
	return nil
}

func (w writer) array(n int, item func(int) any, depth int) error {
	w.b.WriteByte('[')
	for i := range n {
		w.member(i, depth)
		if err := w.value(item(i), depth+1); err != nil {
			return err
		}
	}
	if n > 0 {
		w.close(depth)
	}
	w.b.WriteByte(']')
	return nil
}

func (w writer) key(key string) error {
	if w.o.Marshal {
		return w.marshal(key)
	}
	w.str(key)
	return nil
}

func (w writer) value(value any, depth int) error {
	switch v := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		slices.Sort(keys)
		fields := make(Object, len(keys))
		for i, key := range keys {
			fields[i] = Field{Key: key, Value: v[key]}
		}
		return w.object(fields, depth)
	case []any:
		return w.array(len(v), func(i int) any { return v[i] }, depth)
	case []string:
		return w.array(len(v), func(i int) any { return v[i] }, depth)
	}
	if w.o.Marshal {
		return w.marshal(value)
	}
	switch v := value.(type) {
	case Object:
		return w.object(v, depth)
	case []map[string]any:
		return w.array(len(v), func(i int) any { return v[i] }, depth)
	case string:
		w.str(v)
	case nil:
		w.b.WriteString("null")
	case bool:
		w.b.WriteString(strconv.FormatBool(v))
	case int:
		w.b.WriteString(strconv.Itoa(v))
	case int64:
		w.b.WriteString(strconv.FormatInt(v, 10))
	case *big.Int:
		w.b.WriteString(v.String())
	case float64:
		w.float(v)
	case json.Number:
		return w.number(v)
	default:
		if !w.lenient {
			return fmt.Errorf("unsupported ordered JSON value %T", value)
		}
		fmt.Fprintf(w.b, "%v", v)
	}
	return nil
}

// float writes a float as json.dumps does: its repr, with NaN and the infinities spelled as
// JavaScript spells them.
func (w writer) float(f float64) {
	switch {
	case math.IsNaN(f):
		w.b.WriteString("NaN")
	case math.IsInf(f, 1):
		w.b.WriteString("Infinity")
	case math.IsInf(f, -1):
		w.b.WriteString("-Infinity")
	default:
		w.b.WriteString(Float(f))
	}
}

func (w writer) number(n json.Number) error {
	text := string(n)
	if w.o.Normalize {
		if !strings.ContainsAny(text, ".eE") {
			integer, ok := new(big.Int).SetString(text, 10)
			if !ok {
				return w.invalid(n)
			}
			w.b.WriteString(integer.String())
			return nil
		}
		f, err := strconv.ParseFloat(text, 64)
		if err != nil && !math.IsInf(f, 0) {
			return w.invalid(n)
		}
		w.float(f)
		return nil
	}
	if !w.lenient {
		// encoding/json's own check: an empty Number is 0, anything else must be a JSON number.
		if text == "" {
			text = "0"
		}
		if !validNumber(text) {
			return w.invalid(n)
		}
	}
	w.b.WriteString(text)
	return nil
}

func (w writer) invalid(n json.Number) error {
	if w.lenient {
		w.b.WriteString(string(n))
		return nil
	}
	return fmt.Errorf("encode scalar: json: invalid number literal %q", string(n))
}

// validNumber is encoding/json's isValidNumber: text is one JSON number and nothing else.
func validNumber(s string) bool {
	if s == "" {
		return false
	}
	if s[0] == '-' {
		if s = s[1:]; s == "" {
			return false
		}
	}
	digits := func(s string) string {
		for s != "" && '0' <= s[0] && s[0] <= '9' {
			s = s[1:]
		}
		return s
	}
	switch {
	case s[0] == '0':
		s = s[1:]
	case '1' <= s[0] && s[0] <= '9':
		s = digits(s[1:])
	default:
		return false
	}
	if len(s) >= 2 && s[0] == '.' && '0' <= s[1] && s[1] <= '9' {
		s = digits(s[2:])
	}
	if len(s) >= 2 && (s[0] == 'e' || s[0] == 'E') {
		s = s[1:]
		if s[0] == '+' || s[0] == '-' {
			if s = s[1:]; s == "" {
				return false
			}
		}
		for s != "" && '0' <= s[0] && s[0] <= '9' {
			s = s[1:]
		}
	}
	return s == ""
}

func (w writer) marshal(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if w.o.UnescapeHTML {
		data = bytes.ReplaceAll(data, []byte(`\u003c`), []byte("<"))
		data = bytes.ReplaceAll(data, []byte(`\u003e`), []byte(">"))
		data = bytes.ReplaceAll(data, []byte(`\u0026`), []byte("&"))
	}
	w.b.Write(data)
	return nil
}

// str writes a string as json.dumps writes the str Python holds for it (CodePoint): a WTF-8
// surrogate and a byte that is not UTF-8 are that surrogate's \u escape, never U+FFFD, and with
// ensure_ascii every character past U+007E is escaped too, as a surrogate pair past U+FFFF.
func (w writer) str(s string) {
	b := w.b
	b.WriteByte('"')
	for i := 0; i < len(s); {
		r, size := CodePoint(s, i)
		if IsSurrogate(r) && (w.o.Bytes == ReplacedAll || w.o.Bytes == ReplacedBytes && size == 1) {
			r, size = 0xfffd, 1
		}
		i += size
		switch {
		case r == '"' || r == '\\':
			b.WriteByte('\\')
			b.WriteRune(r)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case IsSurrogate(r):
			fmt.Fprintf(b, `\u%04x`, r)
		case r < 0x20 || !w.o.Unicode && r > 0x7e:
			for _, unit := range utf16.Encode([]rune{r}) {
				fmt.Fprintf(b, `\u%04x`, unit)
			}
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
}
