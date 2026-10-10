package gate

import (
	"encoding/json"
	"strings"
	"testing"
)

// The receipt's exitCode is interpolated as JavaScript's String() spells a number (Number::toString): a literal past the double range is an
// infinity, and 1e21 takes the exponential form. The refusal text is the whole judgement, so it is compared whole.
func TestAReceiptExitCodeIsSpelledAsJavaScriptSpellsANumber(t *testing.T) {
	for _, c := range []struct {
		code any
		text string
	}{
		{json.Number("1e999"), "Infinity"},
		{json.Number("-1e999"), "-Infinity"},
		{json.Number("1e21"), "1e+21"},
		{json.Number("1e-7"), "1e-7"},
		{json.Number("12345678"), "12345678"},
		{1.5, "1.5"},
		{-3, "-3"},
	} {
		got := gateAfter(t, nil, map[string]any{"exitCode": c.code}, nil)
		if want := "the receipt reports exitCode " + c.text + "; only a passing run closes a check"; got.OK || !strings.Contains(got.Reason, want) {
			t.Errorf("exitCode %v: %+v, want a refusal containing %q", c.code, got, want)
		}
	}
}
