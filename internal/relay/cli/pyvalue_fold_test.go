package cli

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The relay CLI's repr, type name, truth and == answer as internal/pyvalue does over the values
// its reader builds.
func TestFoldCLIValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) { return decodeJSON([]byte(doc)) })
	// Its reader (store.LoadsJSON) gives an integer as a json.Number and nothing else as one.
	keep := pyjsontest.Both(pyjsontest.IntegerNumbers, pyjsontest.Types("pyjson.Object", "[]interface {}", "string", "bool", "nil", "float64", "json.Number"))
	pyjsontest.SameText(t, "pyRepr", values, keep, pyRepr, pyvalue.Repr)
	pyjsontest.SameText(t, "pyTypeName", values, keep, pyTypeName, pyvalue.TypeName)
	pyjsontest.SameTruth(t, "truthy", values, keep, truthy, pyvalue.Truthy)
	pyjsontest.SameText(t, "sha256Hex", values, pyjsontest.Types("string"), func(v any) string { return sha256Hex(v.(string)) }, func(v any) string { return pyvalue.SHA256Hex(v.(string)) })
}
