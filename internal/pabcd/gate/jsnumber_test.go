package gate

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

// The receipt's exitCode is interpolated as JavaScript's String() spells a number (Number::toString): a literal past the double range is an
// infinity, and 1e21 takes the exponential form. The refusal sentence is checked as a whole piece of the reason, between the start of the
// reason and the CHECK-BINDING-01 suffix refuse adds.
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
		if want := "the receipt reports exitCode " + c.text + "; only a passing run closes a check (CHECK-BINDING-01)."; got.OK || !strings.HasPrefix(got.Reason, want) {
			t.Errorf("exitCode %v: %+v, want a refusal starting %q", c.code, got, want)
		}
	}
}

// NaN cannot arrive through a receipt file (JSON has no NaN), so the spelling that the shared formatter gives it, where the gate's own
// formatter once wrote the empty string, is pinned on the helper that builds the sentence. It is compared exactly.
func TestExitCodeRefusalSpellsNaNAndTheInfinities(t *testing.T) {
	for _, c := range []struct {
		code float64
		text string
	}{
		{math.NaN(), "NaN"},
		{math.Inf(1), "Infinity"},
		{math.Inf(-1), "-Infinity"},
		{1, "1"},
		{-0.5, "-0.5"},
		{1e21, "1e+21"},
	} {
		if got, want := exitCodeRefusal(c.code), "the receipt reports exitCode "+c.text+"; only a passing run closes a check"; got != want {
			t.Errorf("exitCodeRefusal(%v) = %q, want %q", c.code, got, want)
		}
	}
}
