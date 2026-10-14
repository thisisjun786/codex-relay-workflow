package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
)

// CRW-1449: the refusals a task gets for acting on a relationship or a project it does not hold name the layer it is not, the 자식
// (relay role id parent), and never the bare role id. The held-by form and the short form of the correction read the same up to
// the held-by tail, so the two steps of a correction still give one text.
func TestRelationshipRefusalNamesLayers(t *testing.T) {
	t.Parallel()
	rel := relRow{ID: "rel-1", Status: "active", ParentTaskID: "p1"}
	heldBy := notParentHeldBy("t1", rel).Error()
	short := notParentOf("t1", rel).Error()
	if want := "scope_role_mismatch: task t1 is not the 자식 that holds relationship rel-1, which is held by p1"; heldBy != want {
		t.Errorf("held-by refusal = %q, want %q", heldBy, want)
	}
	if want := "scope_role_mismatch: task t1 is not the 자식 that holds relationship rel-1"; short != want {
		t.Errorf("short refusal = %q, want %q", short, want)
	}
	if !strings.HasPrefix(heldBy, short+", ") {
		t.Errorf("the held-by refusal %q does not extend the short one %q", heldBy, short)
	}

	f := newFixture(t)
	forkJoinPlan(f, "p1")
	f.projectParent()
	ctx := context.Background()
	project := requireProjectParent(ctx, f.s.Q(ctx), "P-TEST", "intruder").Error()
	if want := "scope_role_mismatch: task intruder is not the registered 자식 of project P-TEST"; project != want {
		t.Errorf("project refusal = %q, want %q", project, want)
	}
	for _, text := range []string{heldBy, short, project} {
		if hits := layername.Find(text); len(hits) != 0 {
			t.Errorf("%q names a bare role id: %v", text, hits)
		}
	}
}
