package cli

import (
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// decodeJSON is json.loads into Python-shaped values (store.LoadsJSON).
func decodeJSON(data []byte) (any, error) { return store.LoadsJSON(data) }

func fieldIndex(object contract.OrderedObject, key string) int {
	for i, field := range object {
		if field.Key == key {
			return i
		}
	}
	return -1
}

// get is dict.get: the value under key, or nil when absent or when object is not an object.
func get(object any, key string) any {
	if o, ok := object.(contract.OrderedObject); ok {
		if at := fieldIndex(o, key); at >= 0 {
			return o[at].Value
		}
	}
	return nil
}

func has(object any, key string) bool {
	o, ok := object.(contract.OrderedObject)
	return ok && fieldIndex(o, key) >= 0
}

// pyInt answers type(v) is int for a decoded JSON value (bool excluded).
func pyInt(v any) (int64, bool) {
	number, ok := v.(json.Number)
	if !ok {
		return 0, false
	}
	value, err := number.Int64()
	return value, err == nil
}

// pyTypeName is type(v).__name__ for a decoded JSON value.
func pyTypeName(v any) string {
	switch v.(type) {
	case nil:
		return "NoneType"
	case bool:
		return "bool"
	case string:
		return "str"
	case json.Number:
		return "int"
	case float64:
		return "float"
	case []any:
		return "list"
	default:
		return "dict"
	}
}

// pyRepr is repr() of a decoded JSON value.
func pyRepr(v any) string {
	switch value := v.(type) {
	case nil:
		return "None"
	case bool:
		if value {
			return "True"
		}
		return "False"
	case string:
		return store.PythonRepr(value)
	case json.Number:
		return string(value)
	case float64:
		return pyjson.Float(value)
	case []any:
		parts := make([]string, len(value))
		for i, item := range value {
			parts[i] = pyRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case contract.OrderedObject:
		parts := make([]string, len(value))
		for i, field := range value {
			parts[i] = store.PythonRepr(field.Key) + ": " + pyRepr(field.Value)
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(value)
	}
}

// pyNumber is the numeric value Python's == compares: bool is an int.
func pyNumber(v any) (float64, bool) {
	switch value := v.(type) {
	case bool:
		if value {
			return 1, true
		}
		return 0, true
	case json.Number:
		f, err := strconv.ParseFloat(string(value), 64)
		return f, err == nil || math.IsInf(f, 0)
	case float64:
		return value, true
	case int:
		return float64(value), true
	case int64:
		return float64(value), true
	}
	return 0, false
}

// pyEqual is Python's == over decoded JSON values: 1 == 1.0 == True, dict order ignored.
func pyEqual(a, b any) bool {
	if x, ok := pyNumber(a); ok {
		y, ok := pyNumber(b)
		if ok {
			if ia, aok := a.(json.Number); aok {
				if ib, bok := b.(json.Number); bok {
					return string(ia) == string(ib) || x == y
				}
			}
			return x == y
		}
		return false
	}
	switch value := a.(type) {
	case nil:
		return b == nil
	case string:
		other, ok := b.(string)
		return ok && value == other
	case []any:
		other, ok := b.([]any)
		if !ok || len(other) != len(value) {
			return false
		}
		for i := range value {
			if !pyEqual(value[i], other[i]) {
				return false
			}
		}
		return true
	case contract.OrderedObject:
		other, ok := b.(contract.OrderedObject)
		if !ok || len(other) != len(value) {
			return false
		}
		for _, field := range value {
			at := fieldIndex(other, field.Key)
			if at < 0 || !pyEqual(field.Value, other[at].Value) {
				return false
			}
		}
		return true
	}
	return false
}

// truthy is Python's bool(v) for a decoded JSON value.
func truthy(v any) bool {
	switch value := v.(type) {
	case nil:
		return false
	case bool:
		return value
	case string:
		return value != ""
	case []any:
		return len(value) > 0
	case contract.OrderedObject:
		return len(value) > 0
	}
	n, _ := pyNumber(v)
	return n != 0
}

// nullable renders Python's None for an unmeasured field ("" or 0 in the store's Go types).
func nullableText(v string) any {
	if v == "" {
		return nil
	}
	return v
}

func nullableCount(v uint64) any {
	if v == 0 {
		return nil
	}
	return int64(v)
}
