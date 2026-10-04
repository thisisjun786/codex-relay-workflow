package goalplan

import (
	"encoding/json"
	"strings"
	"testing"
)

// The cases of goalplan.test.ts that a revival can answer (the file reads and writes behind them belong to later issues). Their
// inputs are recorded cases of oracle-plan.json, so the Go answer is held against what the oracle printed for the same text.

func recorded(t *testing.T, name string) recordedPlan {
	t.Helper()
	c, ok := loadOraclePlans(t).Plans[name]
	if !ok {
		t.Fatalf("no recorded case %s", name)
	}
	return c
}

// "schema round-trips" and "legacy plan keeps byte-identical serialized plan data": a plan written in the oracle's key order
// comes back as the same text, for every schema version, with an absent dependsOn staying absent and an empty one staying [].
func TestRevivalKeepsAWrittenPlanByteIdentical(t *testing.T) {
	for _, name := range []string{"minimal", "legacy_plan_without_optional_fields", "version_absent", "version_v1", "version_v2", "version_v3",
		"phase_dependsOn_absent", "phase_dependsOn_empty", "phase_dependsOn_ids", "phase_awaitsDecision_empty", "task_dependsOn_absent", "task_dependsOn_empty",
		"task_dependsOn_ids", "task_outcome_text", "criterion_surface_desktop", "steering_two", "steering_empty", "decisions_empty"} {
		c := recorded(t, name)
		if got := revivedText(t, c.Raw, c.Slug); got != c.Raw {
			t.Errorf("%s: revived\n%s\nwant the text it was written as\n%s", name, got, c.Raw)
		}
	}
}

// "task outcome is trimmed while absent and blank outcomes stay absent", "malformed dependsOn or phase shape rejects the whole plan"
// and "an unsupported future schemaVersion is rejected": the shapes the oracle test lists are refused, each as the oracle did.
func TestRevivalRefusesWholePlanForTheListedShapes(t *testing.T) {
	for _, name := range []string{"phase_dependsOn_string", "phase_dependsOn_non_string", "phase_dependsOn_blank", "task_dependsOn_string", "task_dependsOn_null_item",
		"task_dependsOn_empty_id", "phase_entry_title_missing", "phase_entry_title_number", "version_v4", "version_huge", "version_fraction_3_5",
		"slug_stored_traversal", "slug_mismatch_with_requested", "steering_null", "decisions_null"} {
		c := recorded(t, name)
		if c.Oracle != nil {
			t.Fatalf("%s: the oracle did not refuse", name)
		}
		if got := revivedText(t, c.Raw, c.Slug); got != "null" {
			t.Errorf("%s: revived %s, want a refusal", name, got)
		}
	}
	for name, outcome := range map[string]string{"task_outcome_padded": "node --test: 0 fail", "task_outcome_blank": "", "task_outcome_number": ""} {
		c := recorded(t, name)
		plan := reviveGoalplan(decodePlan(t, c.Raw), &c.Slug)
		if plan == nil || plan.WorkPhases[0].Tasks[0].Outcome != outcome {
			t.Errorf("%s: outcome %+v, want %q", name, plan, outcome)
		}
	}
}

// Without an expected slug the stored one only has to be a slug; with one it has to match.
func TestRevivalExpectedSlug(t *testing.T) {
	plan := decodePlan(t, recorded(t, "minimal").Raw)
	for _, c := range []struct {
		expect *string
		want   bool
	}{{nil, true}, {ptr("rec-plan"), true}, {ptr("other"), false}, {ptr(""), false}} {
		if got := reviveGoalplan(plan, c.expect) != nil; got != c.want {
			t.Errorf("expected slug %v: revived = %v, want %v", c.expect, got, c.want)
		}
	}
}

// A plan reader may hand over float64 numbers or json.Number; both are JavaScript numbers to the revival.
func TestRevivalNumberForms(t *testing.T) {
	base := func(v any) map[string]any {
		return map[string]any{"objective": "o", "slug": "p", "workPhases": []any{}, "criteria": []any{}, "schemaVersion": v}
	}
	for _, c := range []struct {
		v    any
		want float64
		ok   bool
	}{{float64(2.9), 2, true}, {json.Number("2.9"), 2, true}, {json.Number("-0"), 0, true}, {float64(4), 0, false}, {json.Number("1e999"), 0, false}} {
		plan := reviveGoalplan(base(c.v), nil)
		if (plan != nil) != c.ok || (c.ok && (plan.SchemaVersion == nil || *plan.SchemaVersion != c.want)) {
			t.Errorf("schemaVersion %#v: %+v", c.v, plan)
		}
	}
}

// Go decodes a lone surrogate escape to U+FFFD before any reviver runs (revive.go), which the oracle's strings keep. Two options
// that differ only by lone surrogates are duplicates here, so a plan the oracle accepts is refused, and an option and a
// recommendation that differ only by lone surrogates match, so a plan the oracle refuses is accepted (known-defects.md, kept).
func TestRevivalLoneSurrogatesReadAsReplacementCharacters(t *testing.T) {
	decision := func(extra string) string {
		return `{"objective":"\ud800","slug":"p","workPhases":[],"criteria":[],"decisions":[{"id":"d","question":"q","status":"open","askedAt":"2026-01-01T00:00:00.000Z",` + extra + `}]}`
	}
	p := "p"
	if plan := reviveGoalplan(decodePlan(t, decision(`"options":["a"]`)), &p); plan == nil || plan.Objective != "\uFFFD" {
		t.Fatalf("objective: %+v", plan)
	}
	if plan := reviveGoalplan(decodePlan(t, decision(`"options":["\ud800","\ud801"]`)), &p); plan != nil {
		t.Error("two options that differ only by lone surrogates were accepted")
	}
	if plan := reviveGoalplan(decodePlan(t, decision(`"options":["\ud800"],"recommendation":"\ud801"`)), &p); plan == nil || !strings.Contains(compact(t, plan), "\"recommendation\":\"\uFFFD\"") {
		t.Errorf("an option and a recommendation that differ only by lone surrogates: %+v", plan)
	}
}
