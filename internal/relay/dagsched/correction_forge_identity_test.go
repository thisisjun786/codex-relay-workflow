package dagsched

import (
	"context"
	"strings"
	"testing"
)

// CRW-906 generation 2, round 6 d3: an acceptance written before the forge rule keeps the target it was
// accepted against (a local checkout here) while dag_acceptance_forge holds the owner/name a merge turn is
// requested against. The in-flight guard must match the forge identity. A pull-request turn that names no
// relationship and is already merging is otherwise missed, and the correction would be recorded over a head
// the forge may already have merged.
func TestACorrectionIsNotRecordedOverALegacyAcceptanceTurnOfItsForgeRepository(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	// the stored target is a local checkout; the forge identity is the owner/name the turn is requested against
	k.exec("UPDATE dag_acceptances SET repository = '/synthetic/checkout', head_sha = 'head-b', pr_number = 5 WHERE relationship_id = ?", rid)
	k.exec("DELETE FROM dag_acceptance_forge WHERE acceptance_id = (SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?)", rid)
	k.exec("INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES ((SELECT acceptance_id FROM dag_acceptances WHERE relationship_id = ?), 'owner/repo', 5)", rid)
	// a merging turn of the forge repository for the pull request, naming neither a relationship nor the head
	k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, pr_number, candidate_head, state, tenure, requested_at, updated_at)" +
		" VALUES ('mtn-forge', 'tgt', 'owner/repo', 'dev', 'P-TEST', 'parent', 'host', NULL, 5, 'head-other', 'merging', 1, 't', 't')")
	_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a correction over a merging turn of the legacy acceptance's forge repository = %v, want disposition_conflict", err)
	}
	if !strings.Contains(err.Error(), "mtn-forge") {
		t.Fatalf("the refusal does not name the turn: %v", err)
	}
	if got := acExecution(k, "B", 2); got != "" {
		t.Fatalf("the refused correction wrote the execution %q", got)
	}
}

// CRW-906 final round, evaluation 6f79654d D2: a live bundle that carries an accepted head through another
// node is still the bundle whose merge may already be on the forge, so a correction of the accepted node is
// refused when the bundle's member holds that head, whichever relationship the member is.
func TestACorrectionIsNotRecordedOverALiveBundleCarryingTheHeadThroughAnotherNode(t *testing.T) {
	t.Parallel()
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["B"].RelationshipID
	prepared := k.rvPrepare("sr", "B")
	acOpenByHand(t, k, rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	k.exec("UPDATE dag_acceptances SET repository = 'owner/repo', head_sha = 'head-b' WHERE relationship_id = ?", rid)
	k.exec("INSERT INTO merge_trains (train_id, target_key, repository, base_ref, base_sha, leader_task_id, created_at) VALUES ('trn-other', 'tgt', 'owner/repo', 'dev', 'base-0', 'parent', 't')")
	// the member is another relationship that carries the same head
	k.exec("INSERT INTO merge_train_members (train_id, seq, turn_id, pr_number, relationship_id, member_head) VALUES ('trn-other', 1, 'turn-other', 6, 'rel-other-node', 'head-b')")
	k.exec("INSERT INTO merge_train_events (train_id, seq, kind, actor, detail_json, recorded_at) VALUES ('trn-other', 1, 'opened', 'parent', '{}', 't')")
	_, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest)
	if refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a correction over a live bundle carrying the head through another node = %v, want disposition_conflict", err)
	}
	if !strings.Contains(err.Error(), "trn-other") {
		t.Fatalf("the refusal does not name the bundle: %v", err)
	}
	if got := acExecution(k, "B", 2); got != "" {
		t.Fatalf("the refused correction wrote the execution %q", got)
	}
}
