package evidence

import (
	"encoding/json"
	"math"
	"math/big"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// PythonError is a host failure in Python's JSON access expressions, not an
// unreadable transport response. The collector boundary recovers only this type;
// programming panics still propagate. This keeps deeply nested accessors from
// silently turning malformed forge values into empty evidence.
type PythonError struct{ Class, Detail string }

func (e *PythonError) Error() string { return e.Class + ": " + e.Detail }

// RecoverPython recovers only deliberate Python expression failures at a command
// boundary. Runtime/programming panics are not hidden.
func RecoverPython(err *error) {
	if value := recover(); value != nil {
		if failure, ok := value.(*PythonError); ok {
			*err = failure
		} else {
			panic(value)
		}
	}
}

// Dict implements dict access, optionally after Python's `value or {}`.
func Dict(v any, orEmpty bool) map[string]any {
	if orEmpty && !Truthy(v) {
		return map[string]any{}
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if o, ok := v.(contract.OrderedObject); ok {
		m := make(map[string]any, len(o))
		for _, f := range o {
			m[f.Key] = f.Value
		}
		return m
	}
	panic(&PythonError{"AttributeError", "'" + TypeName(v) + "' object has no attribute 'get'"})
}

func forgeObject(v any, orEmpty bool) map[string]any { return Dict(v, orEmpty) }

// Items implements iteration over `value or []`, including strings and
// dictionary keys. Python does not coerce a malformed iterable into an empty list.
func Items(v any) []any {
	return Iter(Or(v, []any{}))
}

// Iter implements iteration without an `or []` fallback. False, zero and None
// are errors here, even though Items deliberately treats them as empty.
func Iter(v any) []any {
	if items, ok := List(v); ok {
		return items
	}
	if s, ok := v.(string); ok {
		out := make([]any, 0, len(s))
		for _, r := range s {
			out = append(out, string(r))
		}
		return out
	}
	if o, ok := Object(v); ok {
		out := make([]any, len(o))
		for i, f := range o {
			out[i] = f.Key
		}
		return out
	}
	panic(&PythonError{"TypeError", "'" + TypeName(v) + "' object is not iterable"})
}

func forgeItems(v any) []any { return Items(v) }

// Item is Python's object[key], distinct from .get for missing/wrong shapes.
func Item(v any, key string) any {
	if o, ok := Object(v); ok {
		if value, present := Lookup(o, key); present {
			return value
		}
		panic(&PythonError{"KeyError", Repr(key)})
	}
	switch TypeName(v) {
	case "str":
		panic(&PythonError{"TypeError", "string indices must be integers, not 'str'"})
	case "list":
		panic(&PythonError{"TypeError", "list indices must be integers or slices, not str"})
	default:
		panic(&PythonError{"TypeError", "'" + TypeName(v) + "' object is not subscriptable"})
	}
}

// Len and Index preserve the operations Python performs before iteration.
func Len(v any) int {
	if items, ok := List(v); ok {
		return len(items)
	}
	if o, ok := Object(v); ok {
		return len(o)
	}
	if s, ok := v.(string); ok {
		return len([]rune(s))
	}
	panic(&PythonError{"TypeError", "object of type '" + TypeName(v) + "' has no len()"})
}
func Index(v any, index int) any {
	if items, ok := List(v); ok {
		if index >= len(items) {
			panic(&PythonError{"IndexError", "list index out of range"})
		}
		return items[index]
	}
	if s, ok := v.(string); ok {
		r := []rune(s)
		if index >= len(r) {
			panic(&PythonError{"IndexError", "string index out of range"})
		}
		return string(r[index])
	}
	if _, ok := Object(v); ok {
		panic(&PythonError{"KeyError", Text(index)})
	}
	panic(&PythonError{"TypeError", "'" + TypeName(v) + "' object is not subscriptable"})
}
func collectionItems(v any) []any {
	v = Or(v, []any{})
	Len(v)
	return Items(v)
}

func forgeRunID(v any) (n *big.Int, valid bool) {
	defer func() {
		if e := recover(); e != nil {
			if p, ok := e.(*PythonError); ok && (p.Class == "TypeError" || p.Class == "ValueError") {
				n, valid = nil, false
			} else {
				panic(e)
			}
		}
	}()
	return Integer(v), true
}

// Or returns Python's `value or fallback` without coercing either operand.
func Or(value, fallback any) any {
	if Truthy(value) {
		return value
	}
	return fallback
}

// Whole tests Python's int type without imposing SQLite's int64 storage bound.
func Whole(value any) (*big.Int, bool) {
	switch v := value.(type) {
	case int, int64, *big.Int:
		return Integer(v), true
	case json.Number:
		n, ok := new(big.Int).SetString(string(v), 10)
		return n, ok
	default:
		return nil, false
	}
}

// Integer implements int(value), including Python's exception class and detail.
func Integer(value any) *big.Int {
	switch v := value.(type) {
	case bool:
		if v {
			return big.NewInt(1)
		}
		return big.NewInt(0)
	case int:
		return big.NewInt(int64(v))
	case int64:
		return big.NewInt(v)
	case *big.Int:
		return new(big.Int).Set(v)
	case json.Number:
		if n, ok := new(big.Int).SetString(string(v), 10); ok {
			return n
		}
	case string:
		if n, ok := argparse.ParseInt(v); ok {
			return n
		}
		panic(&PythonError{"ValueError", "invalid literal for int() with base 10: " + Repr(v)})
	case float64:
		if math.IsNaN(v) {
			panic(&PythonError{"ValueError", "cannot convert float NaN to integer"})
		}
		if math.IsInf(v, 0) {
			panic(&PythonError{"OverflowError", "cannot convert float infinity to integer"})
		}
		n, _ := new(big.Float).SetFloat64(v).Int(nil)
		return n
	}
	panic(&PythonError{"TypeError", "int() argument must be a string, a bytes-like object or a real number, not '" + TypeName(value) + "'"})
}

// HashKey preserves Python's scalar equality (True == 1 == 1.0) and rejects
// mutable dictionary keys instead of stringifying unlike identities together.
func HashKey(value any) string {
	switch v := value.(type) {
	case nil:
		return "none"
	case string:
		return "str:" + v
	case bool, int, int64, json.Number, *big.Int:
		return "num:" + Integer(v).String()
	case float64:
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			if v == math.Trunc(v) {
				return "num:" + Integer(v).String()
			}
		}
		return "num:" + pyjson.Float(v)
	}
	panic(&PythonError{"TypeError", "unhashable type: '" + TypeName(value) + "'"})
}

// Equal compares JSON values using Python equality, without Go interface panics.
func Equal(a, b any) bool {
	if x, ok := Object(a); ok {
		y, ok := Object(b)
		if !ok || len(x) != len(y) {
			return false
		}
		for _, f := range x {
			v, ok := Lookup(y, f.Key)
			if !ok || !Equal(f.Value, v) {
				return false
			}
		}
		return true
	}
	if x, ok := List(a); ok {
		y, ok := List(b)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !Equal(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	if _, ok := Object(b); ok {
		return false
	}
	if _, ok := List(b); ok {
		return false
	}
	return HashKey(a) == HashKey(b)
}

func forgeText(v any) string {
	if !Truthy(v) {
		return ""
	}
	return Text(v)
}

// Decode reads stored JSON without discarding object order or numeric types (pyjson.Loads: an
// integer a json.Number, any other number a float64). Malformed stored documents fail as
// json.loads does, rather than becoming empty; NaN, the infinities and a number past float64's
// range fail as encoding/json refuses them.
func Decode(text string) any {
	value, err := pyjson.Loads(text, pyjson.LoadOptions{Python: true, RangeErrors: true})
	if err != nil {
		panic(&PythonError{"JSONDecodeError", err.Error()})
	}
	return value
}

// decodeForgeJSON reads what the forge printed as encoding/json reads it (pyjson.Loads): ordered
// objects, an integer a json.Number, any other number a float64, a number past float64's range
// refused; no output at all is null.
func decodeForgeJSON(out string) (any, error) {
	if out == "" {
		out = "null"
	}
	return pyjson.Loads(out, pyjson.LoadOptions{RangeErrors: true})
}
