package adapter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The adapter's type name and truth answer as internal/pyvalue does over what encoding/json
// (UseNumber) reads from the host.
func TestFoldAdapterValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		decoder := json.NewDecoder(strings.NewReader(doc))
		decoder.UseNumber()
		var value any
		return value, decoder.Decode(&value)
	})
	// UseNumber leaves no float64.
	keep := pyjsontest.Types("map[string]interface {}", "pyjson.Object", "[]interface {}", "string", "bool", "nil", "json.Number")
	pyjsontest.SameText(t, "pythonType", values, keep, pythonType, pyvalue.TypeName)
	// A cursor is read from what encoding/json decoded, never an Object.
	pyjsontest.SameTruth(t, "cursorTruthy", values, pyjsontest.Types("map[string]interface {}", "[]interface {}", "string", "bool", "nil", "json.Number"), cursorTruthy, pyvalue.Truthy)
}
