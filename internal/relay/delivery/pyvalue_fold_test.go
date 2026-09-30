package delivery

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson/pyjsontest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
)

// delivery's repr, str, type name, truth and == answer as internal/pyvalue does over the values
// its reader builds.
func TestFoldDeliveryValues(t *testing.T) {
	values := pyjsontest.Decoded(pyjsontest.Recorded(t), loads)
	// Its reader refuses NaN and the infinities and gives every number as an int64 or a float64.
	keep := pyjsontest.Both(pyjsontest.Finite, pyjsontest.Types("pyjson.Object", "[]interface {}", "string", "bool", "nil", "int64", "float64"))
	pyjsontest.SameText(t, "pyReprValue", values, keep, pyReprValue, pyvalue.Repr)
	pyjsontest.SameText(t, "pyStr", values, keep, pyStr, pyvalue.Str)
	pyjsontest.SameText(t, "pyTypeName", values, keep, pyTypeName, pyvalue.TypeName)
	text := pyjsontest.Types("string")
	pyjsontest.SameText(t, "pyStrip", values, text, func(v any) string { return pyStrip(v.(string)) }, func(v any) string { return pyvalue.Strip(v.(string)) })
	pyjsontest.SameText(t, "sha256Hex", values, text, func(v any) string { return sha256Hex(v.(string)) }, func(v any) string { return pyvalue.SHA256Hex(v.(string)) })
}
