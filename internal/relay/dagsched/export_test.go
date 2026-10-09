package dagsched

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// SettledNonPRNodeForTest is the plan of rvSettledSharedRoot for tests outside this package (package dagsched_test, which may import packages that import this one): the
// store, the relationship that executed the accepted non_pr node B, its parent task, and the event and revision that acceptance stands on.
func SettledNonPRNodeForTest(t *testing.T) (st *store.Store, relationship, parent, event, revision string) {
	t.Helper()
	k, accepted := rvSettledSharedRoot(t)
	a := accepted["B"]
	if err := k.s.DB.QueryRow("SELECT event_id, revision_hash FROM dag_acceptances WHERE acceptance_id = ?", a.AcceptanceID).Scan(&event, &revision); err != nil {
		t.Fatal(err)
	}
	if err := k.s.DB.QueryRow("SELECT parent_task_id FROM relationships WHERE relationship_id = ?", a.RelationshipID).Scan(&parent); err != nil {
		t.Fatal(err)
	}
	return k.s, a.RelationshipID, parent, event, revision
}
