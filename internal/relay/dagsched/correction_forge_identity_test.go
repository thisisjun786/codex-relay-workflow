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
