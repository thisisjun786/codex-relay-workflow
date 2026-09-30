package pyjson

import (
	"encoding/json"
	"errors"
	"io"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// Numbers is the Go value Loads gives a JSON number.
type Numbers uint8

const (
	// PythonNumbers: an integer is a json.Number as spelled, a number with a fraction or an
	// exponent is a float64, and NaN and the infinities are float64.
	PythonNumbers Numbers = iota
	// Int64Numbers: an integer is an int64, or a json.Number past int64's range; any other
	// number is a float64.
	Int64Numbers
	// BigNumbers: an integer is an int64, or a *big.Int past int64's range; any other number is
	// a float64.
	BigNumbers
	// Int64FloatNumbers: an integer is an int64, or the float64 it rounds to past int64's range;
	// any other number is a float64.
	Int64FloatNumbers
	// SpelledNumbers: every number, NaN and the infinities included, is a json.Number as spelled.
	SpelledNumbers
)

// Trailing is what Loads accepts after the document's value and the whitespace behind it.
type Trailing uint8

const (
	// TrailingNothing: the value is the whole document.
	TrailingNothing Trailing = iota
	// TrailingClose: nothing, or anything starting with ']' or '}' (what json.Decoder.More
	// answers false for).
	TrailingClose
	// TrailingAnything: the first value is read and whatever follows it is left unread.
	TrailingAnything
)

// LoadOptions says which reading of a document Loads gives: json.loads' (Python) or
// encoding/json's, and the Go values it is read into. Every reading keeps an object's key order
// in an Object and gives a repeated key its first position and its last value, as a dict does.
type LoadOptions struct {
	// Python refuses what json.loads refuses (with its JSONDecodeError text, Error) and nothing
	// else. Without it what encoding/json's Decoder refuses is refused, with its error, and NaN,
	// Infinity and -Infinity with it.
	Python bool
	// Constants accepts NaN, Infinity and -Infinity, as json.loads does. A Python reading
	// without it refuses the first of them with encoding/json's error.
	Constants bool
	// Surrogates keeps a lone surrogate escape as the three WTF-8 bytes a Go string holds a lone
	// surrogate in (json.loads' str). Without it the escape is U+FFFD, as encoding/json reads it.
	Surrogates bool
	// RawSurrogates keeps a lone surrogate the document holds in its three WTF-8 bytes, as a
	// text DecodeBytesWTF8 decoded holds it. Without it each of those bytes is U+FFFD, as every
	// other byte that is not UTF-8 is.
	RawSurrogates bool
	// Numbers is the Go value of a number.
	Numbers Numbers
	// RangeErrors refuses a number past float64's range, where it would become a float64, with
	// strconv's error (json.Number.Float64's), rather than reading it as an infinity.
	RangeErrors bool
	// Map reads an object into a map[string]any (a repeated key keeps its last value).
	Map bool
	// Repeats keeps a repeated key as another field of the Object, where a dict keeps one.
	Repeats bool
	// Trailing is what may follow the value.
	Trailing Trailing
	// Deep reads a document however deeply its containers nest, as json.loads does up to its
	// recursion limit, which a caller models with ErrorWithLimit. Without it a container nested
	// deeper than MaxDepth is refused, as encoding/json refuses it ("exceeded max depth").
	Deep bool
}

// MaxDepth is encoding/json's nesting limit: how many containers deep its Decoder reads.
const MaxDepth = 10000

// errDepth is encoding/json's refusal of a container past MaxDepth, as its text reads.
var errDepth = errors.New("exceeded max depth")

// errTrailing is Loads' refusal of what follows the value, for a reading that allows nothing
// there. A Python reading never meets it: Error refuses such a document as "Extra data".
var errTrailing = errors.New("trailing data after the JSON value")

// ErrTrailing reports whether err is Loads' refusal of data after the value.
func ErrTrailing(err error) bool { return errors.Is(err, errTrailing) }

// Loads reads one JSON document into Go values: an Object (or a map[string]any) for an object,
// an []any for an array, a string, a bool, nil, and a number as o.Numbers says.
func Loads(doc string, o LoadOptions) (any, error) {
	if o.Python {
		if message := Error(doc); message != "" {
			return nil, errors.New(message)
		}
	}
	d := &decoder{s: doc, o: o}
	value, err := d.value()
	if err != nil {
		return nil, d.fail(err)
	}
	d.space()
	switch {
	case d.i == len(d.s) || o.Trailing == TrailingAnything:
	case o.Trailing == TrailingClose && (d.s[d.i] == ']' || d.s[d.i] == '}'):
	default:
		return nil, d.fail(errTrailing)
	}
	return value, nil
}

type decoder struct {
	s     string
	i     int
	o     LoadOptions
	depth int
}

// open enters a container, refusing it past MaxDepth unless the reading is Deep.
func (d *decoder) open() error {
	d.depth++
	if !d.o.Deep && d.depth > MaxDepth {
		// A reading that accepts what encoding/json refuses (a Python reading, the constants)
		// read the document through encoding/json only after checking it, so its depth refusal
		// is the first encoding/json meets; any other refuses as encoding/json's own walk does.
		if d.o.Python || d.o.Constants {
			return errDepth
		}
		return errSyntax
	}
	return nil
}

// errSyntax marks a document the scan refuses; fail replaces it with the reader's own refusal.
var errSyntax = errors.New("syntax")

// fail is the error a refused document gets: a Python reading has already refused every
// document json.loads refuses, so what is left is a refusal it makes of its own (a constant, a
// number's range); an encoding/json reading refuses as json.Decoder's token walk does.
func (d *decoder) fail(err error) error {
	if d.o.Python || (err != errSyntax && err != errTrailing) {
		return err
	}
	if walked := goWalk(d.s, d.o.Trailing); walked != nil {
		return walked
	}
	return err
}

func (d *decoder) space() {
	for d.i < len(d.s) && (d.s[d.i] == ' ' || d.s[d.i] == '\t' || d.s[d.i] == '\n' || d.s[d.i] == '\r') {
		d.i++
	}
}

func (d *decoder) value() (any, error) {
	d.space()
	if d.i >= len(d.s) {
		return nil, errSyntax
	}
	rest := d.s[d.i:]
	switch c := rest[0]; {
	case c == '{':
		return d.object()
	case c == '[':
		return d.array()
	case c == '"':
		return d.str()
	case strings.HasPrefix(rest, "null"):
		d.i += 4
		return nil, nil
	case strings.HasPrefix(rest, "true"):
		d.i += 4
		return true, nil
	case strings.HasPrefix(rest, "false"):
		d.i += 5
		return false, nil
	case c == 'N' || c == 'I' || strings.HasPrefix(rest, "-I"):
		return d.constant(rest)
	}
	return d.number()
}

// constant reads NaN, Infinity or -Infinity where the reading accepts them, and otherwise
// refuses the first as encoding/json's scanner does.
func (d *decoder) constant(rest string) (any, error) {
	for _, one := range []struct {
		word  string
		value float64
	}{{"NaN", math.NaN()}, {"Infinity", math.Inf(1)}, {"-Infinity", math.Inf(-1)}} {
		if !strings.HasPrefix(rest, one.word) {
			continue
		}
		if !d.o.Constants {
			if !d.o.Python {
				return nil, errSyntax
			}
			return nil, constantRefusal(one.word)
		}
		d.i += len(one.word)
		if d.o.Numbers == SpelledNumbers {
			return json.Number(one.word), nil
		}
		return one.value, nil
	}
	return nil, errSyntax
}

// constantRefusal is encoding/json's refusal of a constant json.loads accepts.
func constantRefusal(word string) error {
	if word[0] == '-' {
		return errors.New("invalid character 'I' in numeric literal")
	}
	return errors.New("invalid character '" + word[:1] + "' looking for beginning of value")
}

func (d *decoder) object() (any, error) {
	if err := d.open(); err != nil {
		return nil, err
	}
	defer func() { d.depth-- }()
	d.i++
	var fields Object
	var fieldMap map[string]any
	var index map[string]int
	if d.o.Map {
		fieldMap = map[string]any{}
	} else {
		fields = Object{}
	}
	d.space()
	if d.i < len(d.s) && d.s[d.i] == '}' {
		d.i++
		return d.made(fields, fieldMap), nil
	}
	for {
		d.space()
		if d.i >= len(d.s) || d.s[d.i] != '"' {
			return nil, errSyntax
		}
		key, err := d.str()
		if err != nil {
			return nil, err
		}
		d.space()
		if d.i >= len(d.s) || d.s[d.i] != ':' {
			return nil, errSyntax
		}
		d.i++
		item, err := d.value()
		if err != nil {
			return nil, err
		}
		switch {
		case d.o.Map:
			fieldMap[key.(string)] = item
		case d.o.Repeats:
			fields = append(fields, Field{Key: key.(string), Value: item})
		default:
			fields, index = assign(fields, index, key.(string), item)
		}
		d.space()
		if d.i >= len(d.s) {
			return nil, errSyntax
		}
		switch d.s[d.i] {
		case ',':
			d.i++
		case '}':
			d.i++
			return d.made(fields, fieldMap), nil
		default:
			return nil, errSyntax
		}
	}
}

func (d *decoder) made(fields Object, fieldMap map[string]any) any {
	if d.o.Map {
		return fieldMap
	}
	return fields
}

// assign is a dict assignment of a decoded field; index finds a repeated key once an object has
// grown past a few fields, so a document with many keys decodes in time proportional to it.
func assign(fields Object, index map[string]int, key string, value any) (Object, map[string]int) {
	const indexed = 16
	if index == nil && len(fields) >= indexed {
		index = make(map[string]int, 2*len(fields))
		for i, field := range fields {
			if _, seen := index[field.Key]; !seen {
				index[field.Key] = i
			}
		}
	}
	if index != nil {
		if at, seen := index[key]; seen {
			fields[at].Value = value
			return fields, index
		}
		index[key] = len(fields)
		return append(fields, Field{Key: key, Value: value}), index
	}
	for i := range fields {
		if fields[i].Key == key {
			fields[i].Value = value
			return fields, index
		}
	}
	return append(fields, Field{Key: key, Value: value}), index
}

func (d *decoder) array() (any, error) {
	if err := d.open(); err != nil {
		return nil, err
	}
	defer func() { d.depth-- }()
	d.i++
	items := []any{}
	d.space()
	if d.i < len(d.s) && d.s[d.i] == ']' {
		d.i++
		return items, nil
	}
	for {
		item, err := d.value()
		if err != nil {
			return nil, err
		}
		items = append(items, item)
		d.space()
		if d.i >= len(d.s) {
			return nil, errSyntax
		}
		switch d.s[d.i] {
		case ',':
			d.i++
		case ']':
			d.i++
			return items, nil
		default:
			return nil, errSyntax
		}
	}
}

// number reads the JSON number at d.i: -?(0|[1-9][0-9]*)(\.[0-9]+)?([eE][-+]?[0-9]+)?
func (d *decoder) number() (any, error) {
	start := d.i
	digits := func() int {
		from := d.i
		for d.i < len(d.s) && d.s[d.i] >= '0' && d.s[d.i] <= '9' {
			d.i++
		}
		return d.i - from
	}
	if d.i < len(d.s) && d.s[d.i] == '-' {
		d.i++
	}
	switch {
	case d.i < len(d.s) && d.s[d.i] == '0':
		d.i++
	case d.i < len(d.s) && d.s[d.i] >= '1' && d.s[d.i] <= '9':
		digits()
	default:
		return nil, errSyntax
	}
	integer := true
	if d.i < len(d.s) && d.s[d.i] == '.' {
		d.i++
		if digits() == 0 {
			return nil, errSyntax
		}
		integer = false
	}
	if d.i < len(d.s) && (d.s[d.i] == 'e' || d.s[d.i] == 'E') {
		d.i++
		if d.i < len(d.s) && (d.s[d.i] == '+' || d.s[d.i] == '-') {
			d.i++
		}
		if digits() == 0 {
			return nil, errSyntax
		}
		integer = false
	}
	return d.numberValue(d.s[start:d.i], integer)
}

func (d *decoder) numberValue(text string, integer bool) (any, error) {
	if d.o.Numbers == SpelledNumbers || integer && d.o.Numbers == PythonNumbers {
		return json.Number(text), nil
	}
	if integer {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, nil
		}
		switch d.o.Numbers {
		case Int64Numbers:
			return json.Number(text), nil
		case BigNumbers:
			n, _ := new(big.Int).SetString(text, 10)
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil && (d.o.RangeErrors || !math.IsInf(f, 0)) {
		return nil, err
	}
	return f, nil
}

// str reads the string token at d.i.
func (d *decoder) str() (any, error) {
	d.i++
	var b strings.Builder
	for {
		if d.i >= len(d.s) {
			return nil, errSyntax
		}
		c := d.s[d.i]
		switch {
		case c == '"':
			d.i++
			return b.String(), nil
		case c < 0x20:
			return nil, errSyntax
		case c == '\\':
			if err := d.escape(&b); err != nil {
				return nil, err
			}
		case c < utf8.RuneSelf:
			b.WriteByte(c)
			d.i++
		default:
			r, size := utf8.DecodeRuneInString(d.s[d.i:])
			if r == utf8.RuneError && size == 1 {
				if _, wtf8 := wtf8At(d.s, d.i); wtf8 && d.o.RawSurrogates {
					b.WriteString(d.s[d.i : d.i+3])
					d.i += 3
					continue
				}
				b.WriteRune(utf8.RuneError)
				d.i++
				continue
			}
			b.WriteString(d.s[d.i : d.i+size])
			d.i += size
		}
	}
}

// escape reads the escape at d.i into b: a pair of \u escapes that encode one code point is that
// code point, and a lone surrogate escape is kept (Surrogates) or U+FFFD.
func (d *decoder) escape(b *strings.Builder) error {
	if d.i+1 >= len(d.s) {
		return errSyntax
	}
	if simple := strings.IndexByte(`"\/bfnrt`, d.s[d.i+1]); simple >= 0 {
		b.WriteByte("\"\\/\b\f\n\r\t"[simple])
		d.i += 2
		return nil
	}
	unit, ok := hex4(d.s, d.i)
	if !ok {
		return errSyntax
	}
	d.i += 6
	if utf16.IsSurrogate(unit) {
		if low, ok := hex4(d.s, d.i); ok {
			if r := utf16.DecodeRune(unit, low); r != utf8.RuneError {
				b.WriteRune(r)
				d.i += 6
				return nil
			}
		}
		if !d.o.Surrogates {
			b.WriteRune(utf8.RuneError)
			return nil
		}
		b.Write([]byte{byte(0xe0 | unit>>12), byte(0x80 | (unit>>6)&0x3f), byte(0x80 | unit&0x3f)})
		return nil
	}
	b.WriteRune(unit)
	return nil
}

// hex4 reads the \uXXXX escape at s[i:].
func hex4(s string, i int) (rune, bool) {
	if i+6 > len(s) || s[i] != '\\' || s[i+1] != 'u' {
		return 0, false
	}
	value, err := strconv.ParseUint(s[i+2:i+6], 16, 16)
	return rune(value), err == nil
}

// wtf8At reports the lone surrogate s holds at i in its three WTF-8 bytes (ED A0..BF 80..BF).
func wtf8At(s string, i int) (rune, bool) {
	if i+2 >= len(s) || s[i] != 0xed || s[i+1] < 0xa0 || s[i+1] > 0xbf || s[i+2] < 0x80 || s[i+2] > 0xbf {
		return 0, false
	}
	return 0xd000 | rune(s[i+1]&0x3f)<<6 | rune(s[i+2]&0x3f), true
}

// goWalk is the error json.Decoder gives the document: read token by token, as every ordered
// reader built on encoding/json read one, or with one Decode where whatever follows the value is
// left unread (TrailingAnything), as the plain readers read one.
func goWalk(doc string, trailing Trailing) error {
	decoder := json.NewDecoder(strings.NewReader(doc))
	decoder.UseNumber()
	if trailing == TrailingAnything {
		var value any
		return decoder.Decode(&value)
	}
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		for decoder.More() {
			if delim == '{' {
				if _, err := decoder.Token(); err != nil {
					return err
				}
			}
			if err := walk(); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	switch trailing {
	case TrailingClose:
		if decoder.More() {
			return errTrailing
		}
		return nil
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return errTrailing
	}
	return nil
}
