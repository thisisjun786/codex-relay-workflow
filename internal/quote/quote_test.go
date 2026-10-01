package quote

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// A string is Go's quoting, an invisible character escaped; any other value its compact JSON,
// the text unescaped and an object's fields in their order.
func TestValueQuotesAStringAndWritesAnyOtherValueAsJSON(t *testing.T) {
	for _, c := range []struct {
		value any
		want  string
	}{
		{"x\u00a0y", `"x\u00a0y"`},
		{"it's", `"it's"`},
		{nil, "null"},
		{true, "true"},
		{int64(-1), "-1"},
		{[]any{int64(1), "é"}, `[1,"é"]`},
		{pyjson.Object{{Key: "b", Value: nil}, {Key: "a", Value: 2.5}}, `{"b":null,"a":2.5}`},
	} {
		if got := Value(c.value); got != c.want {
			t.Errorf("Value(%#v) = %s, want %s", c.value, got, c.want)
		}
	}
}
