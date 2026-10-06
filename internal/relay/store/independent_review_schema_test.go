package store

import (
	"path/filepath"
	"strings"
	"testing"
)

// independentReviewCases are the cases the optional independentReview item of the completion
// receipt is judged on: the receipts that state it validly, and the ones the contract refuses. A
// receipt that states none stays valid, which is what the 18 cases of draft7-verdicts.json.gz still
// say against the revised schema.
func independentReviewCases(t *testing.T) []schemaCase {
	t.Helper()
	digest := strings.Repeat("a", 64)
	ready := `{"eventId":"0123456789abcdef0123456789abcdef","relationshipId":"rel-synthetic","executionGeneration":1,"attempt":1,"revisionHash":"` + digest + `","outcome":"ready_for_review","producer":"child","turnRef":{"threadId":"synthetic-child","turnId":"synthetic-turn","turnStatus":"inProgress"},"emittedAt":"2026-01-01T00:00:00Z","manifest":[{"path":"/synthetic/handoff.json","sha256":"` + digest + `"}]}`
	item := func(change func(map[string]any)) func(map[string]any) {
		return func(receipt map[string]any) {
			stated := map[string]any{"artifact": map[string]any{"path": "/synthetic/review/artifact.json", "sha256": digest}, "status": "complete", "invalidReviewerCalls": 0,
				"dispositions": []any{map[string]any{"finding": 0, "disposition": "fixed", "evidence": "synthetic commit"}}}
			if change != nil {
				change(stated)
			}
			receipt["independentReview"] = stated
		}
	}
	executionOnly := func(outcome string) func(map[string]any) {
		return func(receipt map[string]any) {
			receipt["outcome"], receipt["manifest"], receipt["revisionHash"] = outcome, nil, strings.Repeat("0", 64)
			item(nil)(receipt)
		}
	}
	var cases []schemaCase
	for _, c := range []struct {
		label  string
		valid  bool
		change func(map[string]any)
	}{
		{"ready without the item", true, func(map[string]any) {}},
		{"complete item", true, item(nil)},
		{"partial item with a reason", true, item(func(i map[string]any) { i["status"], i["reason"] = "partial", "one reviewer failed" })},
		{"unavailable item with no artifact", true, item(func(i map[string]any) {
			i["status"], i["reason"] = "unavailable", "the daily cap stopped the run"
			delete(i, "artifact")
		})},
		{"item with a patch-id and no dispositions", true, item(func(i map[string]any) { i["headPatchId"], i["dispositions"] = strings.Repeat("b", 40), []any{} })},
		{"item on a blocked receipt", false, executionOnly("blocked_needs_input")},
		{"item on a failed receipt", false, executionOnly("failed")},
		{"null item", false, func(r map[string]any) { r["independentReview"] = nil }},
		{"complete without an artifact", false, item(func(i map[string]any) { delete(i, "artifact") })},
		{"partial without a reason", false, item(func(i map[string]any) { i["status"] = "partial" })},
		{"unknown status", false, item(func(i map[string]any) { i["status"] = "done" })},
		{"negative invalid reviewer calls", false, item(func(i map[string]any) { i["invalidReviewerCalls"] = -1 })},
		{"relative artifact path", false, item(func(i map[string]any) { i["artifact"].(map[string]any)["path"] = "review/artifact.json" })},
		{"unknown item member", false, item(func(i map[string]any) { i["verdict"] = "good" })},
		{"unknown disposition", false, item(func(i map[string]any) { i["dispositions"].([]any)[0].(map[string]any)["disposition"] = "ignored" })},
		{"disposition without evidence", false, item(func(i map[string]any) { delete(i["dispositions"].([]any)[0].(map[string]any), "evidence") })},
		{"item without dispositions", false, item(func(i map[string]any) { delete(i, "dispositions") })},
		{"patch-id that is not hex", false, item(func(i map[string]any) { i["headPatchId"] = "same" })},
	} {
		cases = append(cases, schemaCase{Schema: "completion-receipt", Label: c.label, Valid: c.valid, Instance: mutated(t, ready, c.change)})
	}
	return cases
}

func TestIndependentReviewSchemaAcceptsOldAndNewReceipts(t *testing.T) {
	cases := independentReviewCases(t)
	var fixture struct {
		SchemaSHA string          `json:"schemaSha256"`
		Verdicts  []draft7Verdict `json:"verdicts"`
	}
	readFixture(t, "independent-review-verdicts.json", &fixture)
	dir := filepath.Join(repositoryRoot(t), "contract", "schema")
	if problems := schemaPinProblems(dir, map[string]string{"completion-receipt": fixture.SchemaSHA}, map[string]int{"completion-receipt": len(cases)}); len(problems) != 0 {
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
