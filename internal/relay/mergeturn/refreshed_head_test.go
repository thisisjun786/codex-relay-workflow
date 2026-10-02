package mergeturn

import (
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// A parent that refreshed a pull request branch itself claims the turn with a head no child ever
// reported (crw-run's merge readiness, "Refresh the base yourself when only the base moved").
// merge-turn-check compares the claimed head with the head of any work report recorded for the
// assignment, but nothing in the product records one (docs/port/decisions.md section 53), so the
// refreshed head has no report head to disagree with. The test holds that property the procedure
// leans on: with no report recorded the check goes on to its other gates, and with one recorded
// naming the old head it refuses the new head (the case the procedure tells the parent to return).
func TestARefreshedHeadHasNoReportHeadToDisagreeWith(t *testing.T) {
	w, _, r := queueFixture(t)
	heads, err := evidence.CurrentReportHeads(w.ctx, w.s, r)
	if err != nil || len(heads) != 0 {
		t.Fatalf("an assignment with no recorded work report holds no report head, got %v, %v", heads, err)
	}
	turn := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-refreshed", true, ClaimOptions{Relationship: sql.NullString{String: r, Valid: true}}))["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	if _, err := w.check(turn, "head-refreshed", "base-0", ""); err != nil {
		t.Fatalf("the refreshed head was refused: %v", err)
	}

	// the same claim on an assignment that does hold a report naming the old head is refused
	w2, _, r2 := queueFixture(t)
	reportForGrant(t, w2, r2, "event-a", "head-reported", 1, nil)
	turn2 := w2.must(w2.m.Request(w2.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-refreshed", true, ClaimOptions{Relationship: sql.NullString{String: r2, Valid: true}}))["turnId"].(string)
	w2.answer(turn2, alpha.TaskID)
	if _, err := w2.check(turn2, "head-refreshed", "base-0", ""); reasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("a recorded report naming another head must refuse the refreshed head, got %v", err)
	}
}
