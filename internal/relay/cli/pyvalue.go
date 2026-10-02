package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// decodeJSON reads stored JSON into the values the relay reads (store.LoadsJSON), as leniently
// as any writer could have written it.
func decodeJSON(data []byte) (any, error) { return store.LoadsJSON(data) }

// decodeInput reads a JSON document a caller handed this command (an option's value, a file,
// stdin) strictly, as encoding/json reads it (UTF-8 only, no NaN or Infinity, at most 10000
// levels of nesting), into the same values decodeJSON gives.
func decodeInput(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("it is not UTF-8 text")
	}
	var probe any
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, err
	}
	return store.LoadsJSON(data)
}

// jsonKind is quote.Kind. merge_evidence.go still calls it by this name; it goes with that file's
// own rewrite.
func jsonKind(v any) string { return quote.Kind(v) }

// shown is a value as a message quotes it: its JSON text.
func shown(v any) string {
	text, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(text)
}

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
