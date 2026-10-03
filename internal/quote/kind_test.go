package quote

import (
	"encoding/json"
	"math/big"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A decoded value is named by its JSON kind: every number is a number, a Go map and an ordered
// Object are objects, every slice an array.
func TestKindNamesADecodedValueByItsJSONKind(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{
		{nil, "null"},
		{true, "a boolean"},
		{"x", "a string"},
		{[]any{}, "an array"},
		{[]string{"a"}, "an array"},
		{[]map[string]any{{}}, "an array"},
		{pyjson.Object{}, "an object"},
		{map[string]any{}, "an object"},
		{json.Number("1"), "a number"},
		{json.Number("1.5"), "a number"},
		{int64(1), "a number"},
		{1.5, "a number"},
		{big.NewInt(1), "a number"},
	} {
		if got := Kind(c.value); got != c.want {
			t.Errorf("Kind(%#v) = %q, want %q", c.value, got, c.want)
		}
	}
}
