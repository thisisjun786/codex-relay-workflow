package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// relationshipReasonCases are the cases the generations[].reason enum of the shipped relationship
// contract is judged on. CRW-906 records a result that was accepted and is still current as a
// correction, and the reason its generation carries is accepted_result_correction: the reason is
// what notes that route, so the record every schema-conforming consumer reads has to carry a value
// the contract allows. The three reasons the registry's generation-open accepts, and the empty
// reason a returning tenure leaves, are valid; anything else is refused.
func relationshipReasonCases(t *testing.T) []schemaCase {
	t.Helper()
	base := `{"relationshipId":"rel-0123456789abcdef","parent":{"taskId":"01parent-task","hostId":"host-a","cwd":null},` +
		`"child":{"taskId":"01child-task","hostId":"host-a","cwd":null},"issueKey":"CRW-906","status":"active",` +
		`"createdAt":"2026-01-01T00:00:00Z","executionGeneration":2,"generations":[` +
		`{"executionGeneration":1,"dispatchRequestId":"dispatch-1","anchorState":"bound","dispatchTurnId":"turn-dispatch-1","openedAt":"2026-01-01T00:00:00Z","boundAt":"2026-01-01T00:00:00Z","reason":null},` +
		`{"executionGeneration":2,"dispatchRequestId":"dispatch-2","anchorState":"bound","dispatchTurnId":"turn-dispatch-2","openedAt":"2026-01-01T00:00:00Z","boundAt":"2026-01-01T00:00:00Z","reason":"initial_assignment"}` +
		`],"authorizedScope":{"artifactRoots":["/synthetic/work"],"allowedRecipients":["01parent-task"]}}`
	// The reason of the generation the record's current pointer names is the one generation-open wrote.
	reason := func(value any) func(map[string]any) {
		return func(record map[string]any) {
			generations := record["generations"].([]any)
			generations[1].(map[string]any)["reason"] = value
		}
	}
	cases := []schemaCase{}
	for _, c := range []struct {
		label string
		value any
		valid bool
	}{
		{"correction of an accepted current result", "accepted_result_correction", true},
		{"correction of a stale result", "needs_changes_revision", true},
		{"the assignment itself", "initial_assignment", true},
		{"a returning tenure, which leaves the reason empty", nil, true},
		{"a reason the relay never writes", "ruled_again", false},
		{"a reason of the wrong type", 2, false},
	} {
		cases = append(cases, schemaCase{Schema: "relationship", Label: c.label, Valid: c.valid, Instance: mutated(t, base, reason(c.value))})
	}
	return cases
}

func TestRelationshipReasonSchemaAcceptsEveryRecordedReason(t *testing.T) {
	cases := relationshipReasonCases(t)
	var fixture struct {
		SchemaSHA string          `json:"schemaSha256"`
		Verdicts  []draft7Verdict `json:"verdicts"`
	}
	readFixture(t, "relationship-reason-verdicts.json", &fixture)
	dir := filepath.Join(repositoryRoot(t), "contract", "schema")
	if problems := schemaPinProblems(dir, map[string]string{"relationship": fixture.SchemaSHA}, map[string]int{"relationship": len(cases)}); len(problems) != 0 {
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
	// The schema the judgments were made against is the shipped one, and it is the one the other
	// verdicts of this schema were judged against: one contract, one pin.
	if fixture.SchemaSHA != schemaPins["relationship"] {
		t.Fatalf("the reason cases were judged against relationship.json %s, and the schema pins hold %s", fixture.SchemaSHA, schemaPins["relationship"])
	}
}

// TestRelationshipReasonSchemaRefusesAReasonTheContractDoesNotKnow pins the shape of the guard: the
// case above is only evidence while the schema still refuses a reason outside its enum.
func TestRelationshipReasonSchemaRefusesAReasonTheContractDoesNotKnow(t *testing.T) {
	var schema struct {
		Properties struct {
			Generations struct {
				Items struct {
					Properties struct {
						Reason struct {
							Enum []any `json:"enum"`
						} `json:"reason"`
					} `json:"properties"`
				} `json:"items"`
			} `json:"generations"`
		} `json:"properties"`
	}
	raw, err := os.ReadFile(filepath.Join(repositoryRoot(t), "contract", "schema", "relationship.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	allowed := map[any]bool{}
	for _, value := range schema.Properties.Generations.Items.Properties.Reason.Enum {
		allowed[value] = true
	}
	if allowed["ruled_again"] {
		t.Fatal("the contract now allows a reason the relay never writes; a case that judges one refused proves nothing")
	}
	for _, reason := range []string{"initial_assignment", "needs_changes_revision", "accepted_result_correction"} {
		if !allowed[reason] {
			t.Errorf("generation-open accepts %q and the record carries it, but the shipped relationship contract does not allow it: %v", reason, schema.Properties.Generations.Items.Properties.Reason.Enum)
		}
	}
	if !allowed[nil] {
		t.Error("a returning tenure leaves generations.reason empty, which the contract spells null")
	}
}
