package dagsched

import (
	"context"
	"strings"
	"testing"
)

// headSpellingPads are the surrounding characters a stored head may carry. SameCommit trims all of them; the
// SQL that compares the head must agree with it, so a head stored with any of these is still the same commit.
var headSpellingPads = []struct{ name, pad string }{
	{"clean", ""}, {"no-break space", "\u00a0"}, {"ideographic space", "\u3000"}, {"em space", "\u2003"},
	{"next line", "\u0085"}, {"vertical tab", "\v"}, {"carriage return", "\r"},
}

// CRW-906 generation 2, round 7: the in-flight guard of a correction compares a stored merge-turn head with the
// accepted head through the one head definition (SameCommit). A turn that names no relationship is found by its
// head and the acceptance's forge repository alone, so a head stored with a no-break or ideographic space, which
// the SQL trim never removed, must still be found and refuse the correction.
func TestACorrectionIsNotRecordedOverAMergingTurnWhoseHeadIsStoredInAnotherSpelling(t *testing.T) {
	t.Parallel()
	for _, s := range headSpellingPads {
		t.Run(s.name, func(t *testing.T) {
			k, accepted := rvSettledSharedRoot(t)
			rid := accepted["B"].RelationshipID
			prepared := k.rvPrepare("sr", "B")
			acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
			// the acceptance is matched by the repository the turn names: its forge identity, as the forge test does
			k.exec("UPDATE dag_acceptances SET repository = 'owner/repo' WHERE relationship_id = ?", rid)
			k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?)", rid)
			// the fixture's accepted node has no head of its own, so the test gives the active acceptance one
			k.exec("UPDATE dag_acceptances SET head_sha = 'head-b' WHERE relationship_id = ? AND state = 'active'", rid)
			k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, candidate_head, state, tenure, requested_at, updated_at)"+
				" VALUES ('mtn-padded', 'tgt', 'owner/repo', 'dev', 'P-TEST', 'parent', 'host', NULL, ?, 'merging', 1, 't', 't')", s.pad+"head-b"+s.pad)
			_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
			if refusalReason(err) != "disposition_conflict" {
				t.Fatalf("a correction over a merging turn stored with a %s head = %v, want disposition_conflict", s.name, err)
			}
			if !strings.Contains(err.Error(), "mtn-padded") {
				t.Fatalf("the refusal does not name the turn: %v", err)
			}
		})
	}
}

// The live-bundle guard matches a member by its train's repository and its head. The member here belongs to another
// accepted node, so the relationship clause cannot match it and only the padded head decides.
func TestACorrectionIsNotRecordedOverALiveBundleWhoseMemberHeadIsStoredInAnotherSpelling(t *testing.T) {
	t.Parallel()
	for _, s := range headSpellingPads {
		t.Run(s.name, func(t *testing.T) {
			k, accepted := rvSettledSharedRoot(t)
			rid := accepted["B"].RelationshipID
			other := accepted["A"].RelationshipID
			prepared := k.rvPrepare("sr", "B")
			acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
			k.exec("UPDATE dag_acceptances SET repository = 'owner/repo', head_sha = 'head-b' WHERE relationship_id = ?", rid)
			k.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES ('trn-spelled', 'tgt', 'owner/repo', 'dev', 'base-0', 'parent', 't')")
			k.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES ('trn-spelled', 1, 'turn-1', 5, ?, ?)", other, s.pad+"head-b"+s.pad)
			k.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES ('trn-spelled', 1, 'opened', 'parent', '{}', 't')")
			_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
			if refusalReason(err) != "disposition_conflict" {
				t.Fatalf("a correction over a live bundle whose member is stored with a %s head = %v, want disposition_conflict", s.name, err)
			}
			if !strings.Contains(err.Error(), "trn-spelled") {
				t.Fatalf("the refusal does not name the bundle: %v", err)
			}
		})
	}
}
