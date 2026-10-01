// Package pyvalue is Python's repr(), str(), type().__name__, bool() and == over the values
// internal/pyjson reads and the relay builds (nil, bool, str, int as int, int64, *big.Int or an
// integer json.Number, float as float64 or a json.Number with a fraction or an exponent, list as
// []any, []string or []map[string]any, dict as pyjson.Object or map[string]any), and the small
// helpers every package spelled for itself: str.strip(), os.fsdecode and os.fsencode, and the
// hex SHA-256 of a str. A str's repr is StrRepr, the one repr of text.
package pyvalue

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"math/big"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Repr is repr(v). An integer json.Number is written as it is spelled, a map[string]any's keys
// in sorted order (a Go map has lost Python's insertion order), and a value of any other Go type
// as fmt spells it.
func Repr(v any) string {
	switch x := v.(type) {
	case string:
		return StrRepr(x)
	case nil, bool, float64, json.Number, int, int64, *big.Int:
		return Str(x)
	case []string:
		return list(len(x), func(i int) any { return x[i] })
	case []any:
		return list(len(x), func(i int) any { return x[i] })
	case []map[string]any:
		return list(len(x), func(i int) any { return x[i] })
	case pyjson.Object:
		parts := make([]string, len(x))
		for i, field := range x {
			parts[i] = StrRepr(field.Key) + ": " + Repr(field.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case map[string]any:
		return Repr(sorted(x))
	case map[string][]string:
		return Repr(sorted(stringLists(x)))
	}
	return fmt.Sprint(v)
}

// stringLists is a map of string lists (url.Values, a request's query) as the dict it stands for.
func stringLists(m map[string][]string) map[string]any {
	out := make(map[string]any, len(m))
	for key, values := range m {
		out[key] = values
	}
	return out
}

func list(n int, item func(int) any) string {
	parts := make([]string, n)
	for i := range n {
		parts[i] = Repr(item(i))
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// sorted is a map as an Object in its keys' order.
func sorted(m map[string]any) pyjson.Object {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	out := make(pyjson.Object, len(keys))
	for i, key := range keys {
		out[i] = pyjson.Field{Key: key, Value: m[key]}
	}
	return out
}

// Str is str(v): a str itself, None, True and False, a float's repr, an integer's digits, a nil
// *int64 as None, and anything else its repr.
func Str(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case *int64:
		if x == nil {
			return "None"
		}
		return strconv.FormatInt(*x, 10)
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return pyjson.Float(x)
	case json.Number, int, int64, *big.Int:
		return fmt.Sprint(x)
	}
	return Repr(v)
}

// TypeName is type(v).__name__; a json.Number is an int or a float as json.loads would have read
// its spelling, and a Go type Python has no name for is named as fmt's %T names it.
func TypeName(v any) string {
	switch x := v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		if strings.ContainsAny(string(x), ".eE") {
			return "float"
		}
		return "int"
	case int, int64, *big.Int:
		return "int"
	case float64:
		return "float"
	case []any, []string, []map[string]any:
		return "list"
	case pyjson.Object, map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Truthy is bool(v): None, False, zero, an empty str and an empty container are false (NaN is
// true); a value of any other Go type is true.
func Truthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case string:
		return x != ""
	case float64:
		return x != 0
	case json.Number:
		f, _ := x.Float64()
		return f != 0
	case int:
		return x != 0
	case int64:
		return x != 0
	case *big.Int:
		return x.Sign() != 0
	case []any:
		return len(x) > 0
	case []string:
		return len(x) > 0
	case []map[string]any:
		return len(x) > 0
	case []byte:
		return len(x) > 0
	case pyjson.Object:
		return len(x) > 0
	case map[string]any:
		return len(x) > 0
	case map[string][]string:
		return len(x) > 0
	}
	return true
}

// Equal is a == b. An int and a float compare exactly, a bool as 0 or 1; a dict compares by its
// keys whatever their order. json.loads builds every NaN as one object (json.decoder.NaN), so NaN
// is never equal to NaN itself while two lists or dicts holding it compare equal, because a
// container compares an item by identity before equality.
func Equal(a, b any) bool { return equal(a, b, false) }

// ItemEqual is a == b as a container compares two of its items: identity first, so json.loads'
// one NaN equals itself here too (a list's order and its membership tests compare so).
func ItemEqual(a, b any) bool { return equal(a, b, true) }

func equal(a, b any, item bool) bool {
	x, xNumber := numberOf(a)
	y, yNumber := numberOf(b)
	if xNumber || yNumber {
		if !xNumber || !yNumber {
			return false
		}
		if item && x.float && y.float && math.IsNaN(x.real) && math.IsNaN(y.real) {
			return true
		}
		return x.equal(y)
	}
	if m, ok := a.(map[string][]string); ok {
		a = stringLists(m)
	}
	if m, ok := b.(map[string][]string); ok {
		b = stringLists(m)
	}
	if m, ok := a.(map[string]any); ok {
		a = sorted(m)
	}
	if m, ok := b.(map[string]any); ok {
		b = sorted(m)
	}
	switch v := a.(type) {
	case nil:
		return b == nil
	case string:
		w, ok := b.(string)
		return ok && v == w
	case []any, []string, []map[string]any:
		items, others := listOf(v), listOf(b)
		if others == nil || len(items) != len(others) {
			return false
		}
		for i := range items {
			if !equal(items[i], others[i], true) {
				return false
			}
		}
		return true
	case pyjson.Object:
		w, ok := b.(pyjson.Object)
		if !ok || len(v) != len(w) {
			return false
		}
		for _, field := range v {
			other, found := w.Lookup(field.Key)
			if !found || !equal(field.Value, other, true) {
				return false
			}
		}
		return true
	}
	return false
}

// listOf is a list's items, or nil for a value that is not a list.
func listOf(v any) []any {
	switch x := v.(type) {
	case []any:
		return x
	case []string:
		out := make([]any, len(x))
		for i, s := range x {
			out[i] = s
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, m := range x {
			out[i] = m
		}
		return out
	}
	return nil
}

// number is an int (a bool included, which Python compares as 0 and 1) or a float.
type number struct {
	integer *big.Int
	real    float64
	float   bool
}

func numberOf(v any) (number, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return number{integer: big.NewInt(1)}, true
		}
		return number{integer: big.NewInt(0)}, true
	case int:
		return number{integer: big.NewInt(int64(x))}, true
	case int64:
		return number{integer: big.NewInt(x)}, true
	case *big.Int:
		return number{integer: x}, true
	case float64:
		return number{real: x, float: true}, true
	case json.Number:
		return spelled(string(x))
	}
	return number{}, false
}

// spelled is json.loads' number: float(text) with a fraction or an exponent (an overflow is an
// infinity, as float() makes it), int(text) otherwise.
func spelled(text string) (number, bool) {
	if strings.ContainsAny(text, ".eE") {
		value, err := strconv.ParseFloat(text, 64)
		if numeric, ok := err.(*strconv.NumError); err != nil && !(ok && numeric.Err == strconv.ErrRange) {
			return number{}, false
		}
		return number{real: value, float: true}, true
	}
	value, ok := new(big.Int).SetString(text, 10)
	return number{integer: value}, ok
}

// equal compares exactly, an int with a float too, as Python does.
func (x number) equal(y number) bool {
	switch {
	case !x.float && !y.float:
		return x.integer.Cmp(y.integer) == 0
	case x.float && y.float:
		return x.real == y.real
	case x.float:
		return floatEqualsInt(x.real, y.integer)
	}
	return floatEqualsInt(y.real, x.integer)
}

func floatEqualsInt(real float64, integer *big.Int) bool {
	if math.IsNaN(real) || math.IsInf(real, 0) {
		return false
	}
	return new(big.Float).SetFloat64(real).Cmp(new(big.Float).SetInt(integer)) == 0
}

// Strip is str.strip(): Unicode White_Space plus the ASCII information separators
// U+001C..U+001F, which str.isspace counts and unicode.IsSpace does not.
func Strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f })
}

// SHA256Hex is hashlib.sha256(text.encode()).hexdigest() of text's bytes.
func SHA256Hex(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// FSDecode is os.fsdecode as Go holds the result: the path's bytes, each byte that is not UTF-8
// replaced by the lone surrogate surrogateescape makes of it (U+DC80..U+DCFF) in WTF-8, which the
// JSON writers spell \udcXX as json.dumps does.
func FSDecode(p string) string {
	if utf8.ValidString(p) {
		return p
	}
	var b strings.Builder
	for i := 0; i < len(p); {
		r, size := utf8.DecodeRuneInString(p[i:])
		if r == utf8.RuneError && size == 1 {
			v := 0xdc00 + rune(p[i])
			b.Write([]byte{0xed, byte(0xa0 | (v>>6)&0x1f), byte(0x80 | v&0x3f)})
			i++
			continue
		}
		b.WriteString(p[i : i+size])
		i += size
	}
	return b.String()
}

// FSEncode is os.fsencode of a string FSDecode makes: each WTF-8 surrogate U+DC80..U+DCFF back to
// the byte it escapes. ok is false for any other lone surrogate, which names no file (Python
// raises UnicodeEncodeError).
func FSEncode(value string) (path string, ok bool) {
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c == 0xed && i+2 < len(value) && value[i+1] >= 0xa0 && value[i+1] <= 0xbf && value[i+2] >= 0x80 && value[i+2] <= 0xbf {
			r := rune(c&0x0f)<<12 | rune(value[i+1]&0x3f)<<6 | rune(value[i+2]&0x3f)
			if r < 0xdc80 || r > 0xdcff {
				return "", false
			}
			out = append(out, byte(r-0xdc00))
			i += 2
			continue
		}
		out = append(out, c)
	}
	return string(out), true
}
