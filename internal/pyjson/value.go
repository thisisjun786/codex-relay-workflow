package pyjson

import (
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Field is one entry of a Python dict.
type Field struct {
	Key   string
	Value any
}

// Object is a Python dict: its fields in insertion order, which a Go map cannot keep. Loads
// builds one for every JSON object and Dumps writes one in that order.
type Object []Field

// Lookup is dict lookup: the value under key and whether the key is present.
func (o Object) Lookup(key string) (any, bool) {
	for _, field := range o {
		if field.Key == key {
			return field.Value, true
		}
	}
	return nil, false
}

// Get is dict.get(key): the value under key, nil when it is absent.
func (o Object) Get(key string) any {
	value, _ := o.Lookup(key)
	return value
}

// Set is dict assignment: a present key keeps its place and takes value, a new one is appended.
func (o Object) Set(key string, value any) Object {
	for i := range o {
		if o[i].Key == key {
			o[i].Value = value
			return o
		}
	}
	return append(o, Field{Key: key, Value: value})
}

// Float is float.__repr__ (and str() of a float): the shortest spelling that reads back as f,
// fixed notation for decimal exponents from -4 up to but not including 16 (with ".0" when it
// has no fraction), scientific notation with at least two exponent digits outside them, and
// nan, inf and -inf.
func Float(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	// A float below 1e-4 or from 1e16 up has a shortest spelling outside that exponent range,
	// and every other float one inside it: both bounds are floats whose shortest spelling is
	// exactly the power of ten.
	if magnitude := math.Abs(f); magnitude == 0 || magnitude >= 1e-4 && magnitude < 1e16 {
		text := strconv.FormatFloat(f, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	// Go's shortest scientific spelling already writes Python's: a signed exponent of at least
	// two digits ("1e+16", "5e-324").
	return strconv.FormatFloat(f, 'e', -1, 64)
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

// IsSurrogate reports whether r is a UTF-16 surrogate code point, which CodePoint gives for a
// lone surrogate and for a byte that is not UTF-8.
func IsSurrogate(r rune) bool { return r >= 0xd800 && r <= 0xdfff }
