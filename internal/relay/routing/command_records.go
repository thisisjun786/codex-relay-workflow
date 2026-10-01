package routing

import "github.com/thisisjun786/codex-relay-workflow/internal/contract"

// CommandRecord is a routing command's answer as the relay CLI prints it: every map as an
// object with its keys in sorted order.
func CommandRecord(value any) any { return sortedRecord(value) }

func sortedRecord(value any) any {
	if m, ok := value.(map[string]any); ok {
		out := contract.OrderedObject{}
		for _, k := range sortedKeys(m) {
			out = append(out, contract.Field{Key: k, Value: sortedRecord(m[k])})
		}
		return out
	}
	if values, ok := value.([]Object); ok {
		out := []any{}
		for _, v := range values {
			out = append(out, sortedRecord(v))
		}
		return out
	}
	if values, ok := value.([]any); ok {
		out := []any{}
		for _, v := range values {
			out = append(out, sortedRecord(v))
		}
		return out
	}
	return value
}
