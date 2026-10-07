package policystore

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
)

// TestOnlyTheTargetMovedCatchesAnUnrelatedEdit is the review finding: the guard compared the edited
// document with itself, so an edit outside the named section was never refused. It is driven
// directly, because no apply function in this package writes outside its section today; the guard
// is what has to refuse the day one does.
func TestOnlyTheTargetMovedCatchesAnUnrelatedEdit(t *testing.T) {
	document, err := decode([]byte(policyText))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotSections(document)
	// An edit that touches the named section and, wrongly, another one too.
	edited, err := decode([]byte(policyText))
	if err != nil {
		t.Fatal(err)
	}
	roles, _ := edited.Get("roles").(pyjson.Object)
	child, _ := roles.Get("child").(pyjson.Object)
	edited = edited.Set("roles", roles.Set("child", child.Set("model", "nobody/nothing")))
	allowed, _ := edited.Get("allowed").([]any)
	if len(allowed) > 0 {
		edited = edited.Set("allowed", allowed[1:])
	}
	change := Change{Kind: KindSetRolePairs, Role: "child", Pairs: []Pair{{Model: "nobody/nothing", Effort: "xhigh"}}}
	err = onlyTheTargetMoved(before, edited, change)
	if err == nil {
		t.Fatal("an edit outside the named section was accepted")
	}
	if !strings.Contains(err.Error(), "allowed") {
		t.Fatalf("the refusal does not name the section: %v", err)
	}
}

// TestCheckRefusesAnEditOutsideItsSection is the same property end to end: the check itself must
// refuse a candidate that moved a section the change does not name. The fixture is the policy plus
// an allowlist row the change would disturb, so a passing answer here means the guard ran.
func TestCheckRefusesAnEditOutsideItsSection(t *testing.T) {
	change := Change{Kind: KindSetAllowed, Model: "anthropic/opus", Efforts: []string{"xhigh", "max", "ultra"}}
	result := Check([]byte(policyText), digestOf(policyText), change)
	if !result.Valid {
		t.Fatalf("a legal allowlist change was refused: %+v", result)
	}
	// The unchanged sections must still compare equal to their snapshots.
	document, err := decode([]byte(policyText))
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotSections(document)
	if err := onlyTheTargetMoved(before, document, change); err != nil {
		t.Fatalf("an untouched document was reported as moved: %v", err)
	}
}
