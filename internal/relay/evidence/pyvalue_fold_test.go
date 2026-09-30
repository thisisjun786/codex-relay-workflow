package evidence

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// evidence's repr, str, type name, truth and == answer as internal/pyvalue does over the values
// its readers build and every Go value the corpus holds.
func TestFoldEvidenceValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		return pyjson.Loads(doc, pyjson.LoadOptions{Python: true, RangeErrors: true})
	})
	// Its readers give an integer as a json.Number and nothing else as one, never a *big.Int or
	// bytes, and refuse NaN and the infinities.
	keep := pyjsontest.Both(pyjsontest.IntegerNumbers, pyjsontest.Types("pyjson.Object", "map[string]interface {}", "map[string][]string",
		"[]interface {}", "[]string", "[]map[string]interface {}", "string", "bool", "nil", "json.Number", "int", "int64", "float64", "*int64"))
	pyjsontest.SameText(t, "Repr", values, keep, Repr, pyvalue.Repr)
	pyjsontest.SameText(t, "Text", values, keep, Text, pyvalue.Str)
	pyjsontest.SameText(t, "TypeName", values, keep, TypeName, pyvalue.TypeName)
	pyjsontest.SameTruth(t, "Truthy", values, keep, Truthy, pyvalue.Truthy)
	seven := int64(7)
	pyjsontest.SameText(t, "Text", []any{&seven}, nil, Text, pyvalue.Str)
	// Equal compares as a container compares its items (HashKey reads every NaN alike).
	pyjsontest.SameEquality(t, "Equal", values, pyjsontest.Both(pyjsontest.IntegerNumbers, func(v any) bool { _, pointer := v.(*int64); _, bytes := v.([]byte); return !pointer && !bytes }), Equal, pyvalue.ItemEqual)
}

func TestFoldEvidenceSHA256(t *testing.T) {
	pyjsontest.SameText(t, "sha256String", pyjsontest.Values(), pyjsontest.Types("string"), func(v any) string { return sha256String(v.(string)) }, func(v any) string { return pyvalue.SHA256Hex(v.(string)) })
}
