// Subset ported for todo 26; todo 24 owns and extends this.

package evidence

import (
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
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
