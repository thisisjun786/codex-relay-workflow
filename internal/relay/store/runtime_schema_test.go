package store

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

func runtimeAttemptCases(t *testing.T) []schemaCase {
	t.Helper()
	base := `{"requestId":"del-0123456789ab-a1","eventId":"0123456789abcdef0123456789abcdef","attemptNo":1,"recipientTaskId":"synthetic-parent","deliveryState":"held_uncertain","sendAttempted":"unknown","retrySafe":false,"observedAt":"2026-01-01T00:00:00Z"}`
	cases := []schemaCase{{Schema: "delivery-attempt", Label: "legacy without runtime", Instance: json.RawMessage(base), Valid: true}}
	for _, c := range []struct {
		label string
		value any
		valid bool
	}{
		{"recorded runtime", map[string]any{"build": "synthetic-build", "executable": "/synthetic/bin/crw"}, true},
		{"unknown executable", map[string]any{"build": "synthetic-build", "executable": nil}, true},
		{"null runtime", nil, false},
		{"wrong runtime type", "build", false},
		{"missing build", map[string]any{"executable": nil}, false},
		{"missing executable", map[string]any{"build": "synthetic-build"}, false},
		{"empty build", map[string]any{"build": "", "executable": nil}, false},
		{"wrong build type", map[string]any{"build": true, "executable": nil}, false},
		{"relative executable", map[string]any{"build": "synthetic-build", "executable": "bin/crw"}, false},
		{"wrong executable type", map[string]any{"build": "synthetic-build", "executable": 1}, false},
		{"extra runtime key", map[string]any{"build": "synthetic-build", "executable": nil, "extra": true}, false},
	} {
		cases = append(cases, schemaCase{Schema: "delivery-attempt", Label: c.label, Valid: c.valid, Instance: mutated(t, base, func(record map[string]any) { record["runtime"] = c.value })})
	}
	return cases
}

func TestRuntimeAttemptSchemaAcceptsOldAndNewRecords(t *testing.T) {
	cases := runtimeAttemptCases(t)
	var fixture struct {
		SchemaSHA string          `json:"schemaSha256"`
		Verdicts  []draft7Verdict `json:"verdicts"`
	}
	readFixture(t, "runtime-attempt-verdicts.json", &fixture)
	dir := filepath.Join(repositoryRoot(t), "contract", "schema")
	if problems := schemaPinProblems(dir, map[string]string{"delivery-attempt": fixture.SchemaSHA}, map[string]int{"delivery-attempt": len(cases)}); len(problems) != 0 {
		t.Fatal(problems)
	}
	if len(fixture.Verdicts) != len(cases) {
		t.Fatalf("%d recorded judgments, want %d", len(fixture.Verdicts), len(cases))
	}
	for i, c := range cases {
		verdict := fixture.Verdicts[i]
		if verdict.Schema != c.Schema || verdict.Label != c.Label || verdict.Instance != normalInstance(t, c.Instance) || (len(verdict.Errors) == 0) != c.Valid {
			t.Fatalf("%s: independent Draft 7 judgment does not match case: %+v", c.Label, verdict)
		}
	}
}
