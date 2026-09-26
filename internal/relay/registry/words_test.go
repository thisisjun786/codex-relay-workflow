package registry

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

// Test25_words_outside_RefusalReason_are_pythons: every state, action, observation word, source
// and prose string assignment/dispositions/rolepolicy emit outside errors.RefusalReason equals
// the Python module's own value (testdata/python_words.json from gen_words.py).
func Test25_words_outside_RefusalReason_are_pythons(t *testing.T) {
	raw, err := os.ReadFile("testdata/python_words.json")
	if err != nil {
		t.Fatal(err)
	}
	var py map[string]any
	if err := json.Unmarshal(raw, &py); err != nil {
		t.Fatal(err)
	}
	next := map[string]any{}
	for k, v := range nextAction {
		next[k] = v
	}
	byState := map[string]any{}
	for k, v := range observationByState {
		byState[k] = v
	}
	details := map[string]any{}
	for k, v := range observationDetail {
		details[k] = v
	}
	for name, got := range map[string]any{
		"NEXT_ACTION":                 next,
		"LIFECYCLE_WITHHOLD_SOURCE":   LifecycleWithholdSource,
		"PARENT_RECOVERY_THEN":        ParentRecoveryThen,
		"CORRECTION_ACTIONS":          []any{ActionCorrectionUnsent, ActionCorrectionUnconfirmed, ActionCorrectionHeld, ActionCorrectionAnswered},
		"OBSERVATION_BY_STATE":        byState,
		"OBSERVATION_DETAIL":          details,
		"UNMEASURED_DETAIL":           UnmeasuredDetail,
		"RECIPIENT_UNMEASURED_DETAIL": RecipientUnmeasuredDetail,
		"DISPOSITION_READING_LIMITS":  DispositionReadingLimits,
		"EXECUTION_ONLY":              anyStrings(executionOnly),
		"RECOVERY":                    RoleRecovery,
		"SETTINGS_DIFFER_AFTER_LOAD":  SettingsDifferAfterLoad,
	} {
		if !reflect.DeepEqual(got, py[name]) {
			t.Errorf("%s: go %v, python %v", name, got, py[name])
		}
	}
}
