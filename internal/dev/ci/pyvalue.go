//go:build dev

package ci

import (
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// pyDict is a decoded JSON object in Python's dict order (first occurrence, last value).
type pyDict struct {
	keys []string
	vals map[string]any
}

func (d *pyDict) get(key string) any { return d.vals[key] }

func (d *pyDict) has(key string) bool {
	_, ok := d.vals[key]
	return ok
}

func (d *pyDict) sortedKeys() []string { return sortedSet(append([]string(nil), d.keys...)) }

// asDict is isinstance(value, dict).
func asDict(value any) (*pyDict, bool) {
	d, ok := value.(*pyDict)
	return d, ok
}

// pyInt reads a JSON number Python parses as an int.
func pyInt(value any) (*big.Int, bool) {
	number, ok := value.(json.Number)
	if !ok || !isPythonInt(number) {
		return nil, false
	}
	n, ok := new(big.Int).SetString(string(number), 10)
	return n, ok
}

// pyFloat reads a JSON number Python parses as a float.
func pyFloat(value json.Number) float64 {
	switch value {
	case "NaN":
		return math.NaN()
	case "Infinity":
		return math.Inf(1)
	case "-Infinity":
		return math.Inf(-1)
	}
	f, _ := strconv.ParseFloat(string(value), 64) // out of range saturates to ±Inf, as Python does
	return f
}

// pyFloatRepr is repr(float).
func pyFloatRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	e := strconv.FormatFloat(f, 'e', -1, 64) // [-]d[.ddd]e±XX
	sign := ""
	if strings.HasPrefix(e, "-") {
		sign, e = "-", e[1:]
	}
	mantissa, expText, _ := strings.Cut(e, "e")
	exp, _ := strconv.Atoi(expText)
	digits := strings.Replace(mantissa, ".", "", 1)
	if exp < -4 || exp >= 16 {
		out := digits[:1]
		if len(digits) > 1 {
			out += "." + digits[1:]
		}
		expSign := "+"
		if exp < 0 {
			expSign, exp = "-", -exp
		}
		return fmt.Sprintf("%s%se%s%02d", sign, out, expSign, exp)
	}
	point := exp + 1
	switch {
	case point <= 0:
		return sign + "0." + strings.Repeat("0", -point) + digits
	case point >= len(digits):
		return sign + digits + strings.Repeat("0", point-len(digits)) + ".0"
	default:
		return sign + digits[:point] + "." + digits[point:]
	}
}

// pyReprValue is repr() of a value json.loads produced.
func pyReprValue(value any) string {
	switch v := value.(type) {
	case nil:
		return "None"
	case bool:
		if v {
			return "True"
		}
		return "False"
	case json.Number:
		if n, ok := pyInt(v); ok {
			return n.String()
		}
		return pyFloatRepr(pyFloat(v))
	case string:
		return pyRepr(v)
	case []any:
		parts := make([]string, len(v))
		for i, item := range v {
			parts[i] = pyReprValue(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case *pyDict:
		parts := make([]string, len(v.keys))
		for i, key := range v.keys {
			parts[i] = pyRepr(key) + ": " + pyReprValue(v.vals[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	panic(fmt.Sprintf("pyReprValue: %T", value))
}

// pyStr is str(value).
func pyStr(value any) string {
	if s, ok := value.(string); ok {
		return s
	}
	return pyReprValue(value)
}

// pyNumber is a JSON number or bool as Python compares it (True == 1 == 1.0).
func pyNumber(value any) (*big.Float, bool, bool) {
	switch v := value.(type) {
	case bool:
		if v {
			return big.NewFloat(1), false, true
		}
		return big.NewFloat(0), false, true
	case json.Number:
		if n, ok := pyInt(v); ok {
			return new(big.Float).SetInt(n), false, true
		}
		f := pyFloat(v)
		if math.IsNaN(f) {
			return nil, true, true
		}
		return big.NewFloat(f), false, true
	}
	return nil, false, false
}

// pyEqual is Python's == between two decoded JSON values.
func pyEqual(a, b any) bool {
	if x, xNaN, ok := pyNumber(a); ok {
		y, yNaN, ok := pyNumber(b)
		return ok && !xNaN && !yNaN && x.Cmp(y) == 0
	}
	switch x := a.(type) {
	case nil:
		return b == nil
	case string:
		y, ok := b.(string)
		return ok && x == y
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !pyEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	case *pyDict:
		y, ok := b.(*pyDict)
		if !ok || len(x.keys) != len(y.keys) {
			return false
		}
		for _, key := range x.keys {
			if !y.has(key) || !pyEqual(x.vals[key], y.vals[key]) {
				return false
			}
		}
		return true
	}
	return false
}

// pyRepr is repr() of a str.
func pyRepr(text string) string {
	quote := "'"
	if strings.Contains(text, "'") && !strings.Contains(text, `"`) {
		quote = `"`
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range text {
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
		case r == ' ' || (r < 0x7f && r > 0x20) || (r >= 0xa0 && unicode.IsPrint(r)):
			b.WriteRune(r)
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

// utf8Subpart measures the undecodable sequence at data[i] the way CPython's UTF-8 decoder
// does: the lead byte plus the continuation bytes that could still have completed it.
func utf8Subpart(data []byte, i int) (int, string) {
	lead := data[i]
	need, low, high := 0, byte(0x80), byte(0xbf)
	switch {
	case lead >= 0xc2 && lead <= 0xdf:
		need = 2
	case lead == 0xe0:
		need, low = 3, 0xa0
	case lead == 0xed:
		need, high = 3, 0x9f
	case lead >= 0xe1 && lead <= 0xef:
		need = 3
	case lead == 0xf0:
		need, low = 4, 0x90
	case lead == 0xf4:
		need, high = 4, 0x8f
	case lead >= 0xf1 && lead <= 0xf3:
		need = 4
	default:
		return 1, "invalid start byte"
	}
	n := 1
	for ; n < need; n++ {
		if i+n >= len(data) {
			return n, "unexpected end of data"
		}
		c := data[i+n]
		if c < low || c > high {
			return n, "invalid continuation byte"
		}
		low, high = 0x80, 0xbf
	}
	return n, "invalid continuation byte"
}

// decodeUTF8 is bytes.decode() in strict mode, with Python's UnicodeDecodeError text.
func decodeUTF8(data []byte) (string, error) {
	if utf8.Valid(data) {
		return string(data), nil
	}
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			n, reason := utf8Subpart(data, i)
			where := fmt.Sprintf("byte 0x%02x in position %d", data[i], i)
			if n > 1 {
				where = fmt.Sprintf("bytes in position %d-%d", i, i+n-1)
			}
			return "", valueError{"'utf-8' codec can't decode " + where + ": " + reason}
		}
		i += size
	}
	return string(data), nil
}

// decodeReplace is bytes.decode(errors="replace").
func decodeReplace(data []byte) string {
	var b strings.Builder
	for i := 0; i < len(data); {
		r, size := utf8.DecodeRune(data[i:])
		if r == utf8.RuneError && size <= 1 {
			n, _ := utf8Subpart(data, i)
			b.WriteRune(utf8.RuneError)
			i += n
			continue
		}
		b.WriteRune(r)
		i += size
	}
	return b.String()
}

func bigInt(n int64) *big.Int { return big.NewInt(n) }
