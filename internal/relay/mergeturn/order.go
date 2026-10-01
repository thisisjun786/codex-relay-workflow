package mergeturn

import (
	"maps"
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// plain turns an answer of this package, built from maps, into the ordered objects and lists
// the relay CLI prints: a map's keys in sorted order.
func plain(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(contract.OrderedObject, 0, len(x))
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out = append(out, contract.Field{Key: k, Value: plain(x[k])})
		}
		return out
	case contract.OrderedObject:
		out := make(contract.OrderedObject, len(x))
		for i, f := range x {
			out[i] = contract.Field{Key: f.Key, Value: plain(f.Value)}
		}
		return out
	case []map[string]any:
		return list(x)
	case []contract.OrderedObject:
		return list(x)
	case []string:
		return list(x)
	case []any:
		return list(x)
	}
	return v
}

func list[T any](items []T) []any {
	out := make([]any, len(items))
	for i, item := range items {
		out[i] = plain(item)
	}
	return out
}
