// Subset ported for todo 26; todo 24 owns and extends this.

package evidence

import (
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/settings"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// Python value semantics for the decoded JSON a caller restates: an object is a
// contract.OrderedObject or a map[string]any, an integer is a json.Number, int or int64, a
// number with a fraction is a float64, and a list is []any or []map[string]any.

// Object reads a decoded JSON object as an ordered one.
func Object(v any) (contract.OrderedObject, bool) {
	switch o := v.(type) {
	case contract.OrderedObject:
		return o, true
	case map[string][]string:
		values := make(map[string]any, len(o))
		for k, v := range o {
			values[k] = v
		}
		return Object(values)
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
	case json.Number, int, int64, *big.Int:
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
	case *int64:
		if x == nil {
			return "None"
		}
		return Text(*x)
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

// Repr is repr(v).
func Repr(v any) string {
	switch x := v.(type) {
	case string:
		return StrRepr(x)
	case nil, bool, float64, json.Number, int, int64, *big.Int:
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
