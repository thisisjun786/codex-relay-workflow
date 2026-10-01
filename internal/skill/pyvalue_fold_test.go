package skill

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// The hook host's truth, str and repr answer as internal/pyvalue does over what it reads.
func TestFoldHookHostValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), func(doc string) (any, error) {
		var value any
		return value, json.NewDecoder(strings.NewReader(doc)).Decode(&value)
	})
	keep := pyjsontest.Both(pyjsontest.Finite, pyjsontest.Types("map[string]interface {}", "[]interface {}", "string", "bool", "nil", "int64", "float64"))
	pyjsontest.SameTruth(t, "hostTruthy", values, keep, hostTruthy, pyvalue.Truthy)
	pyjsontest.SameText(t, "hostPythonString", values, keep, hostPythonString, pyvalue.Str)
	pyjsontest.SameText(t, "hostPythonRepr", values, keep, hostPythonRepr, pyvalue.Repr)
}
