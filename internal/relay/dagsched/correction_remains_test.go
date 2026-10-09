package dagsched

import (
	"context"
	"testing"
)

// CRW-1010 item 1: the in-flight guard matches a pull request number only inside the repository of the acceptance, so a turn of another repository that happens to carry the same number
// (and another head, and no relationship) is not the accepted head on its way to the base and does not hold the correction back.
func TestACorrectionIsNotRefusedForTheSamePullRequestNumberInAnotherRepository(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	k.exec("UPDATE dag_acceptances SET repository = 'owner/repo', head_sha = 'head-b', pr_number = 5 WHERE relationship_id = ?", rid)
	for i, state := range []string{"merging", "unknown", "landed"} {
		k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)"+
			" VALUES (?, ?, 'someone/else', 'dev', 'P-TEST', 'parent', 'host', NULL, 5, 'head-other', ?, 1, 't', 't')", "mtn-other-"+state, "tgt-other-"+string(rune('a'+i)), state)
	}
	res, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if err != nil || res.Generation != 2 {
		t.Fatalf("a correction with the same pull request number in another repository = %v %+v, want it recorded", err, res)
	}
}

// CRW-1010 item 2: an acceptance written before the forge rule (a local checkout as its target, no forge row) reaches the accepted-current route when nothing of it is on its way to the base: the
// relationship clause of the in-flight guard looks at turns, not at the spelling of the acceptance's target.
func TestACorrectionOfALegacyLocalAcceptanceIsRecordedWhenNothingIsOnItsWay(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	k.exec("UPDATE dag_acceptances SET repository = '/synthetic/checkout', head_sha = 'head-b', pr_number = 5 WHERE relationship_id = ?", rid)
	k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?)", rid)
	// a merging turn of the same pull request number in a repository the acceptance does not name is not the accepted head on its way
	k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)" +
		" VALUES ('mtn-elsewhere', 'tgt', 'someone/else', 'dev', 'P-TEST', 'parent', 'host', NULL, 5, 'head-other', 'merging', 1, 't', 't')")
	res, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if err != nil || res.Generation != 2 {
		t.Fatalf("a correction of a legacy local acceptance = %v %+v, want it recorded", err, res)
	}
}

// CRW-1010 item 4: step 4 of the correction procedure in docs/relay/dag-scheduler.md says the edges that take the result of an accepted node read `blocked:stale_head` while the corrected generation
// is not accepted. That is the reading of an edge (B-09, edges.go: the relationship moved to a later generation than the acceptance stands on), not a name of the merge judge, and the doc says so in
// its edge table; this pins the reading the sentence names.
func TestTheEdgesOfAnAcceptedNodeUnderAHandOpenedCorrectionReadBlockedStaleHead(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	if st := k.status("sr", "ac"); !st.Satisfied {
		t.Fatalf("the edge before the correction = %+v", st)
	}
	prepared := k.rvPrepare("sr", "A")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	if _, err := k.sched.RecordCorrection(context.Background(), "sr", "A", "parent", prepared.ManifestDigest); err != nil {
		t.Fatal(err)
	}
	if st := k.status("sr", "ac"); st.Satisfied || st.Reason != BlockedStaleHead {
		t.Fatalf("the edge after the correction is recorded = %+v, want %s", st, BlockedStaleHead)
	}
}
