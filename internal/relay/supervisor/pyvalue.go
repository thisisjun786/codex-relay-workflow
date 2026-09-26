// Subset ported for todo 26; todo 24 owns and extends this.

package supervisor

import (
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Python value semantics for the decoded JSON a caller restates: an object is a
// contract.OrderedObject or a map[string]any, an integer is a json.Number, int or int64, a
// number with a fraction is a float64, and a list is []any or []map[string]any.

// Dumps is json.dumps(value) with the separators, sort_keys and ensure_ascii Python passes.
func Dumps(value any, compact, sortKeys, ascii bool) string {
	var b strings.Builder
	d := dumper{b: &b, item: ", ", key: ": ", sortKeys: sortKeys, ascii: ascii}
	if compact {
		d.item, d.key = ",", ":"
	}
	d.write(value)
	return b.String()
}

type dumper struct {
	b               *strings.Builder
	item, key       string
	sortKeys, ascii bool
}

func (d dumper) write(value any) {
	switch v := value.(type) {
	case contract.OrderedObject:
		fields := v
		if d.sortKeys {
			fields = slices.Clone(v)
			slices.SortStableFunc(fields, func(x, y contract.Field) int { return strings.Compare(x.Key, y.Key) })
		}
		d.b.WriteByte('{')
		for i, f := range fields {
			if i > 0 {
				d.b.WriteString(d.item)
			}
			d.str(f.Key)
			d.b.WriteString(d.key)
			d.write(f.Value)
		}
		d.b.WriteByte('}')
	case map[string]any:
		// A Go map has no insertion order; every caller passing one asks for sorted keys.
		keys := make([]string, 0, len(v))
		for k := range v {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		o := make(contract.OrderedObject, 0, len(keys))
		for _, k := range keys {
			o = append(o, contract.Field{Key: k, Value: v[k]})
		}
		d.write(o)
	case []any:
		d.b.WriteByte('[')
		for i, item := range v {
			if i > 0 {
				d.b.WriteString(d.item)
			}
			d.write(item)
		}
		d.b.WriteByte(']')
	case []string:
		items := make([]any, len(v))
		for i, s := range v {
			items[i] = s
		}
		d.write(items)
	case []map[string]any:
		items := make([]any, len(v))
		for i, m := range v {
			items[i] = m
		}
		d.write(items)
	case string:
		d.str(v)
	case bool:
		d.b.WriteString(strconv.FormatBool(v))
	case nil:
		d.b.WriteString("null")
	case json.Number:
		d.b.WriteString(v.String())
	case int:
		d.b.WriteString(strconv.Itoa(v))
	case int64:
		d.b.WriteString(strconv.FormatInt(v, 10))
	case float64:
		switch {
		case math.IsNaN(v):
			d.b.WriteString("NaN")
		case math.IsInf(v, 1):
			d.b.WriteString("Infinity")
		case math.IsInf(v, -1):
			d.b.WriteString("-Infinity")
		default:
			d.b.WriteString(Float(v))
		}
	default:
		fmt.Fprintf(d.b, "%v", v)
	}
}

func (d dumper) str(s string) {
	d.b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			d.b.WriteString(`\"`)
		case r == '\\':
			d.b.WriteString(`\\`)
		case r == '\n':
			d.b.WriteString(`\n`)
		case r == '\r':
			d.b.WriteString(`\r`)
		case r == '\t':
			d.b.WriteString(`\t`)
		case r == '\b':
			d.b.WriteString(`\b`)
		case r == '\f':
			d.b.WriteString(`\f`)
		case r < 0x20 || (d.ascii && r > 0x7e):
			if r > 0xffff {
				r -= 0x10000
				fmt.Fprintf(d.b, `\u%04x\u%04x`, 0xd800+(r>>10), 0xdc00+(r&0x3ff))
			} else {
				fmt.Fprintf(d.b, `\u%04x`, r)
			}
		default:
			d.b.WriteRune(r)
		}
	}
	d.b.WriteByte('"')
}

// Float is float.__repr__.
func Float(v float64) string {
	magnitude := math.Abs(v)
	if magnitude == 0 || magnitude >= 1e-4 && magnitude < 1e16 {
		text := strconv.FormatFloat(v, 'f', -1, 64)
		if !strings.Contains(text, ".") {
			text += ".0"
		}
		return text
	}
	text := strconv.FormatFloat(v, 'e', -1, 64)
	mantissa, exponent, _ := strings.Cut(text, "e")
	sign := exponent[0]
	digits := strings.TrimLeft(exponent[1:], "0")
	if len(digits) < 2 {
		digits = fmt.Sprintf("%02s", digits)
	}
	return mantissa + "e" + string(sign) + digits
}

// Object reads a decoded JSON object as an ordered one.
func Object(v any) (contract.OrderedObject, bool) {
	switch o := v.(type) {
	case contract.OrderedObject:
		return o, true
	case map[string]any:
		keys := make([]string, 0, len(o))
		for k := range o {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		out := make(contract.OrderedObject, 0, len(keys))
		for _, k := range keys {
			out = append(out, contract.Field{Key: k, Value: o[k]})
		}
		return out, true
	}
	return nil, false
}

// Get is dict.get(key).
func Get(o contract.OrderedObject, key string) any {
	v, _ := Lookup(o, key)
	return v
}

// Lookup is dict lookup: the value and whether the key is present.
func Lookup(o contract.OrderedObject, key string) (any, bool) {
	for _, f := range o {
		if f.Key == key {
			return f.Value, true
		}
	}
	return nil, false
}

// List reads a decoded JSON list.
func List(v any) ([]any, bool) {
	switch l := v.(type) {
	case []any:
		return l, true
	case []string:
		out := make([]any, len(l))
		for i, s := range l {
			out[i] = s
		}
		return out, true
	case []map[string]any:
		out := make([]any, len(l))
		for i, m := range l {
			out[i] = m
		}
		return out, true
	}
	return nil, false
}

// PyInt is _is_int: an int and not a bool.
func PyInt(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	case int:
		return int64(n), true
	case int64:
		return n, true
	}
	return 0, false
}

// TypeName is type(v).__name__.
func TypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number, int, int64:
		return "int"
	case float64:
		return "float"
	case []any, []string, []map[string]any:
		return "list"
	case contract.OrderedObject, map[string]any:
		return "dict"
	}
	return fmt.Sprintf("%T", v)
}

// Text is str(v).
func Text(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "None"
	case bool:
		if x {
			return "True"
		}
		return "False"
	case float64:
		return Float(x)
	case json.Number, int, int64:
		return fmt.Sprint(x)
	}
	return Repr(v)
}

// Repr is repr(v).
func Repr(v any) string {
	switch x := v.(type) {
	case string:
		return StrRepr(x)
	case nil, bool, float64, json.Number, int, int64:
		return Text(x)
	case []string:
		parts := make([]string, len(x))
		for i, s := range x {
			parts[i] = StrRepr(s)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if l, ok := List(v); ok {
		parts := make([]string, len(l))
		for i, item := range l {
			parts[i] = Repr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	}
	if o, ok := Object(v); ok {
		parts := make([]string, len(o))
		for i, f := range o {
			parts[i] = StrRepr(f.Key) + ": " + Repr(f.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return fmt.Sprint(v)
}

// Truthy is bool(v).
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
	}
	if l, ok := List(v); ok {
		return len(l) > 0
	}
	if o, ok := Object(v); ok {
		return len(o) > 0
	}
	return true
}

// IntOf is int(v) for the numbers a restated record carries, with ok false where Python
// would raise.
func IntOf(v any) (int64, bool) {
	switch x := v.(type) {
	case bool:
		if x {
			return 1, true
		}
		return 0, true
	case float64:
		return int64(x), true
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(x), 10, 64)
		return i, err == nil
	}
	return PyInt(v)
}

// StrRepr is repr() of a str.
func StrRepr(s string) string { return settings.Repr(s) }
