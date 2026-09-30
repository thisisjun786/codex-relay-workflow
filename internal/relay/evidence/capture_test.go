package evidence

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// whole compares a scenario's whole output with its golden, keyed by the scenario id.
func whole(t *testing.T, id string, got any) {
	t.Helper()
	golden.CheckJSON(t, id, captureObjects(got))
}

// The semantic capture retains JSON object shape when production carries ordered records.
func captureObjects(value any) any {
	switch v := value.(type) {
	case contract.OrderedObject:
		out := map[string]any{}
		for _, f := range v {
			out[f.Key] = captureObjects(f.Value)
		}
		return out
	case map[string]any:
		out := map[string]any{}
		for k, item := range v {
			out[k] = captureObjects(item)
		}
		return out
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = captureObjects(item)
		}
		return out
	default:
		return value
	}
}
func problemRows(ps []Problem) []any {
	out := make([]any, len(ps))
	for i, p := range ps {
		out[i] = []any{p.Code, p.Detail, p.Incumbent}
	}
	return out
}
func errorRow(value any, err error) any {
	if err == nil {
		return map[string]any{"ok": value}
	}
	return map[string]any{"error": typeName(err), "reason": nil, "detail": err.Error()}
}
func typeName(err error) string {
	if _, ok := err.(*ForgeUsage); ok {
		return "ForgeUsage"
	}
	return "error"
}
