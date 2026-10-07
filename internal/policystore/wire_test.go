package policystore

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestChangeDecodesTheDocumentSpellings is the review finding: the wire keys are the lower-case
// spellings the policy document uses, so a pair carrying reasoningEffort is not read as an empty
// effort.
func TestChangeDecodesTheDocumentSpellings(t *testing.T) {
	raw := "{\"kind\":\"setRolePairs\",\"role\":\"child\",\"pairs\":[{\"model\":\"anthropic/opus\",\"reasoningEffort\":\"xhigh\"}]}"
	var change Change
	if err := json.Unmarshal([]byte(raw), &change); err != nil {
		t.Fatal(err)
	}
	if change.Kind != KindSetRolePairs || change.Role != "child" {
		t.Fatalf("change = %+v", change)
	}
	if len(change.Pairs) != 1 || change.Pairs[0] != (Pair{Model: "anthropic/opus", Effort: "xhigh"}) {
		t.Fatalf("pairs = %+v", change.Pairs)
	}
	// The pair marshals back to the document spelling.
	encoded, err := json.Marshal(change.Pairs[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), "reasoningEffort") {
		t.Fatalf("a pair marshals as %s", encoded)
	}
	// The shorter alias is accepted as well.
	var alias Change
	if err := json.Unmarshal([]byte("{\"kind\":\"setRolePairs\",\"role\":\"child\",\"pairs\":[{\"model\":\"m\",\"effort\":\"high\"}]}"), &alias); err != nil {
		t.Fatal(err)
	}
	if alias.Pairs[0].Effort != "high" {
		t.Fatalf("the alias was not read: %+v", alias.Pairs[0])
	}
}

// TestSetExceptionKeepsTheRecordedRoleScope is the review finding: an exception whose scope is not
// restated keeps the scope it had, rather than becoming unscoped and matching any role.
func TestSetExceptionKeepsTheRecordedRoleScope(t *testing.T) {
	change := Change{Kind: KindSetException, ID: "legacy", Model: "devin/swe-2", Effort: "high", CWD: []string{"/tmp/project"}}
	result := Check([]byte(exceptionWithReason), digestOf(exceptionWithReason), change)
	if !result.Valid {
		t.Fatalf("the change was refused: %+v", result)
	}
	candidate, err := candidateOf(t, exceptionWithReason, change)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(candidate, "\"role\": \"child\"") {
		t.Fatalf("the candidate dropped the exception scope: %s", candidate)
	}
}
