package dagsched

import (
	"context"
	"testing"
)

// The words of the refusals the relationship and parent checks share are the relay's output and do not move: the held-by form (accept, merge judgement, integration observation), the short form
// (correction, withdrawal), and the project parent's.
func TestRelationshipAndParentRefusalsKeepTheirWords(t *testing.T) {
	rel := relRow{ID: "rel-1", Status: "active", ParentTaskID: "p1"}
	if got := relationshipState(relRow{Status: "paused", Superseded: true}); got != "superseded" {
		t.Errorf("a superseded relationship is named %q", got)
	}
	if got := relationshipState(relRow{Status: "cancelled"}); got != "cancelled" {
		t.Errorf("a cancelled relationship is named %q", got)
	}
	if got, want := notParentHeldBy("t1", rel).Error(), "scope_role_mismatch: task t1 is not the parent of relationship rel-1, which is held by p1"; got != want {
		t.Errorf("held-by refusal = %q, want %q", got, want)
	}
	if got, want := notParentOf("t1", rel).Error(), "scope_role_mismatch: task t1 is not the parent of relationship rel-1"; got != want {
		t.Errorf("short refusal = %q, want %q", got, want)
	}

	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	ctx := context.Background()
	q := f.s.Q(ctx)
	if err := requireProjectParent(ctx, q, "P-TEST", "parent"); err != nil {
		t.Errorf("the registered parent is refused: %v", err)
	}
	if got, want := requireProjectParent(ctx, q, "P-TEST", "intruder").Error(), "scope_role_mismatch: task intruder is not the registered parent of project P-TEST"; got != want {
		t.Errorf("project parent refusal = %q, want %q", got, want)
	}
	if err := requireProjectParent(ctx, q, "P-NONE", "parent"); refusalReason(err) != "scope_role_mismatch" {
		t.Errorf("a project with no parent: %v, want scope_role_mismatch", err)
	}
}
