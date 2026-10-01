package registry

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The registry's repr, type name and truth answer as internal/pyvalue does over the values its
// reader builds and every Go value the corpus holds.
func TestFoldRegistryValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) { return decodeJSON([]byte(doc)) })
	// Its reader gives an integer as a json.Number and nothing else as one.
	keep := pyjsontest.Both(pyjsontest.IntegerNumbers, pyjsontest.Types("pyjson.Object", "[]interface {}", "[]string", "string", "bool", "nil", "int64", "int", "float64", "json.Number"))
	pyjsontest.SameText(t, "pyRepr", values, keep, pyRepr, pyvalue.Repr)
	pyjsontest.SameText(t, "pyTypeName", values, keep, pyTypeName, pyvalue.TypeName)
	// truthyValue reads a SQLite column.
	pyjsontest.SameTruth(t, "truthyValue", values, pyjsontest.Types("nil", "bool", "int64", "float64", "string", "[]uint8"), truthyValue, pyvalue.Truthy)
	pyjsontest.SameText(t, "sha256Hex", values, pyjsontest.Types("string"), func(v any) string { return sha256Hex(v.(string)) }, func(v any) string { return pyvalue.SHA256Hex(v.(string)) })
}
