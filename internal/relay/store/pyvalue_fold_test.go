package store

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The frozen manifest's repr, str, type name, truth and == answer as internal/pyvalue does over
// the values its reader builds and every Go value the corpus holds.
func TestFoldStoreValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		return pyjson.Loads(doc, pyjson.LoadOptions{Python: true, Constants: true, Surrogates: true, Numbers: pyjson.BigNumbers})
	})
	// The manifest's values: its reader gives an integer as an int64 or a *big.Int, and hook.Decode
	// (whose values the fence also reads) a json.Number only past int64.
	keep := pyjsontest.Both(bigNumbers, pyjsontest.Types("pyjson.Object", "[]interface {}", "string", "bool", "nil", "int64", "*big.Int", "float64", "json.Number"))
	pyjsontest.SameText(t, "pythonReprValue", values, keep, pythonReprValue, pyvalue.Repr)
	pyjsontest.SameText(t, "PythonStr", values, keep, PythonStr, pyvalue.Str)
	pyjsontest.SameText(t, "pythonTypeName", values, keep, pythonTypeName, pyvalue.TypeName)
	pyjsontest.SameTruth(t, "pythonTruthy", values, keep, pythonTruthy, pyvalue.Truthy)
	pyjsontest.SameEquality(t, "PythonEqual", values, keep, PythonEqual, pyvalue.Equal)
	text := pyjsontest.Types("string")
	pyjsontest.SameText(t, "PythonStrip", values, text, func(v any) string { return PythonStrip(v.(string)) }, func(v any) string { return pyvalue.Strip(v.(string)) })
	pyjsontest.SameText(t, "FSDecode", values, text, func(v any) string { return FSDecode(v.(string)) }, func(v any) string { return pyvalue.FSDecode(v.(string)) })
}

// bigNumbers keeps a json.Number only as hook.Decode leaves one: an integer past int64.
func bigNumbers(v any) bool {
	n, ok := v.(json.Number)
	if !ok {
		return true
	}
	_, err := strconv.ParseInt(string(n), 10, 64)
	return !strings.ContainsAny(string(n), ".eE") && err != nil
}
