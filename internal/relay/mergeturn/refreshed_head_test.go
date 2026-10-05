package mergeturn

import (
	"database/sql"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// A parent that refreshed a pull request branch itself claims the turn with a head no child ever reported (crw-run's merge
// readiness, "Refresh the base yourself when only the base moved"). A claim that names the pull request on a forge repository
// is decided by the forge. A claim that names only the assignment has nothing but the assignment's work reports to compare
// the refreshed head with (CRW-586), and nothing in the product records one (docs/port/decisions.md section 53): merge-turn-check
// refuses it, and says to claim with --pr, until a report names the head. A report naming the old head refuses the refreshed
// head (the case the procedure tells the parent to return).
func TestARefreshedHeadOfARelationshipOnlyClaimNeedsAReportNamingIt(t *testing.T) {
	w, _, r := queueFixture(t)
	heads, err := evidence.CurrentReportHeads(w.ctx, w.s, r)
	if err != nil || len(heads) != 0 {
		t.Fatalf("an assignment with no recorded work report holds no report head, got %v, %v", heads, err)
	}
	turn := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-refreshed", true, ClaimOptions{Relationship: sql.NullString{String: r, Valid: true}}))["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	if _, err := w.check(turn, "head-refreshed", "base-0", ""); reasonOf(err) != "merge_target_unreadable" {
		t.Fatalf("a refreshed head with no report to compare it with must be refused, got %v", err)
	}

	// the same claim on an assignment that does hold a report naming the old head is refused as another candidate
	w2, _, r2 := queueFixture(t)
	reportForGrant(t, w2, r2, "event-a", "head-reported", 1, nil)
	turn2 := w2.must(w2.m.Request(w2.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-refreshed", true, ClaimOptions{Relationship: sql.NullString{String: r2, Valid: true}}))["turnId"].(string)
	w2.answer(turn2, alpha.TaskID)
	if _, err := w2.check(turn2, "head-refreshed", "base-0", ""); reasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("a recorded report naming another head must refuse the refreshed head, got %v", err)
	}

	// and the head a report names is the one the check takes
	w3, _, r3 := queueFixture(t)
	reportForGrant(t, w3, r3, "event-a", "head-refreshed", 1, nil)
	turn3 := w3.must(w3.m.Request(w3.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-refreshed", true, ClaimOptions{Relationship: sql.NullString{String: r3, Valid: true}}))["turnId"].(string)
	w3.answer(turn3, alpha.TaskID)
	if _, err := w3.check(turn3, "head-refreshed", "base-0", ""); err != nil {
		t.Fatalf("the head a report names was refused: %v", err)
	}
}
