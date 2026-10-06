package goalplan

import "testing"

// finalGateJSNumber is JavaScript's Number::toString, not Go's fixed notation: the schemaVersion
// refusal interpolates it, so a value at or past 1e21 (or below 1e-6) must print as the oracle does.
// 1e21 is the case the oracle's suite reaches: dev printed 1000000000000000000000 where JavaScript
// prints 1e+21. The value 4 is the ordinary case and stays fixed.
func TestFinalGateJSNumberMatchesJavaScript(t *testing.T) {
	for _, tc := range []struct {
		in   float64
		want string
	}{
		{1e-7, "1e-7"},
		{4, "4"},
		{3.5, "3.5"},
		{1e21, "1e+21"},
	} {
		if got := finalGateJSNumber(tc.in); got != tc.want {
			t.Errorf("finalGateJSNumber(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A schemaVersion past the supported maximum is refused before any other check; its text interpolates
// the version through finalGateJSNumber, so 1e21 must read 1e+21 in both the full and the structural
// refusal. validate_test.go keeps the ordinary schemaVersion 4 row unchanged.
func TestValidateGoalplanRefusesANewerSchemaVersionAsJavaScriptPrintsIt(t *testing.T) {
	plan := validatePlan(validateVersion(1e21), validatePhase("wp1", WorkPhasePending))
	validateReasonsEqual(t, ValidateGoalplan(plan, nil).Reasons,
		"schemaVersion 1e+21 is newer than this build supports (max 3) - upgrade crw before validating this plan")
	if ValidateGoalplan(plan, nil).OK {
		t.Fatal("ok = true, want false")
	}
	validateReasonsEqual(t, ValidateGoalplanStructuralReasons(plan),
		"schemaVersion 1e+21 is newer than this build supports")
}
