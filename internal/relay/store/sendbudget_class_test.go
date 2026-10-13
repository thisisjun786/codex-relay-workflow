package store

import (
	"context"
	"testing"
)

// CRW-1334 (CRW-1314 design section 3): the relationship ids the upward supervisor channel stages
// under - "run:<id>" (a flow run waking its orchestrator) and "initiative:<key>" (a lead reporting to
// the management seat above its initiative) - have no relationships row, so the class read from the
// parent's binding never applied to them and they were held to the ordinary cap of twelve an hour.
// They are of class orchestrator: one run or one initiative reports every milestone and decision of
// everything below it through that one id. A relationship row, a "project:" id and an unknown id keep
// the class the binding gives them.
func TestRelationshipIsOrchestrator_run_and_initiative_ids_are_of_class_orchestrator(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	for _, c := range []struct {
		relationship string
		want         bool
	}{
		{"run:run-1334", true},
		{"initiative:INI-1", true},
		{"project:PRJ-1", false},
		{"rel-unknown", false},
		{"running", false},
		{"initiatives", false},
	} {
		got, err := s.RelationshipIsOrchestrator(ctx, c.relationship)
		if err != nil {
			t.Fatal(err)
		}
		if got != c.want {
			t.Errorf("RelationshipIsOrchestrator(%q) = %v, want %v", c.relationship, got, c.want)
		}
	}
}
