package acceptance

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-906 generation 2: the correction gate both the merge train and the single-lane judgement read. A
// relationship whose live execution generation is later than the generation its active acceptance stands
// on has a correction open over an accepted result that has not been accepted over yet, and the accepted
// head is not a merge candidate. These tests pin the reading on temporary stores, including the two
// cases that must NOT read as a correction: a recorded base refresh (which moves the stand generation
// with the live one) and a withdrawn generation (which moves the live generation back).

// ucStore is a temporary store with one relationship and its acceptance, at the generations given.
func ucStore(t *testing.T, relationship string, acceptanceGeneration, liveGeneration int64) store.Querier {
	t.Helper()
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
		" VALUES (?, 'ISS-1', 'active', 'task-parent', 'host-p', 'task-child', 'host-c', ?, '[]', '[\"task-parent\"]', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')",
		relationship, liveGeneration); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-1','p','n','m',?,?,'ev-1','rev-1','c','verified','head-1','owner/repo',1,'bound','t','{}','task-parent',0,'2026-10-01T00:00:00Z','active')",
		relationship, acceptanceGeneration); err != nil {
		t.Fatal(err)
	}
	q := s.Querier(ctx)
	return q
}

// TestUnderCorrectionReadsTheLiveGenerationAgainstTheStand: the gate is exactly "the live generation is
// later than the generation the acceptance stands on".
func TestUnderCorrectionReadsTheLiveGenerationAgainstTheStand(t *testing.T) {
	t.Run("the acceptance stands on the live generation", func(t *testing.T) {
		q := ucStore(t, "rel-1", 1, 1)
		under, live, stand, err := UnderCorrection(context.Background(), q, "rel-1")
		if err != nil {
			t.Fatal(err)
		}
		if under || live != 1 || stand != 1 {
			t.Fatalf("UnderCorrection = under %v, live %d, stand %d; want false, 1, 1", under, live, stand)
		}
	})
	t.Run("a correction generation is open", func(t *testing.T) {
		q := ucStore(t, "rel-1", 1, 2)
		under, live, stand, err := UnderCorrection(context.Background(), q, "rel-1")
		if err != nil {
			t.Fatal(err)
		}
		if !under || live != 2 || stand != 1 {
			t.Fatalf("UnderCorrection = under %v, live %d, stand %d; want true, 2, 1", under, live, stand)
		}
	})
	t.Run("the acceptance moved past the correction generation", func(t *testing.T) {
		// dag-accept --supersedes of the corrected result: both moved, so nothing is under correction
		q := ucStore(t, "rel-1", 2, 2)
		under, _, _, err := UnderCorrection(context.Background(), q, "rel-1")
		if err != nil {
			t.Fatal(err)
		}
		if under {
			t.Fatal("an accepted-over correction still reads under correction")
		}
	})
	t.Run("the relationship has no active acceptance", func(t *testing.T) {
		s := openStore(t)
		ctx := context.Background()
		if _, err := s.DB.ExecContext(ctx, "INSERT INTO relationships (relationship_id, issue_key, status, parent_task_id, parent_host_id, child_task_id, child_host_id, execution_generation, artifact_roots, allowed_recipients, created_at, updated_at)"+
			" VALUES ('rel-none', 'ISS-1', 'active', 'task-parent', 'host-p', 'task-child', 'host-c', 2, '[]', '[\"task-parent\"]', '2026-10-01T00:00:00Z', '2026-10-01T00:00:00Z')"); err != nil {
			t.Fatal(err)
		}
		q := s.Querier(ctx)
		under, live, stand, err := UnderCorrection(ctx, q, "rel-none")
		if err != nil {
			t.Fatal(err)
		}
		// no acceptance is the caller's own refusal, not this one: the gate must not claim it
		if under || stand != 0 || live != 2 {
			t.Fatalf("UnderCorrection with no acceptance = under %v, live %d, stand %d; want false, 2, 0", under, live, stand)
		}
	})
}

// TestUnderCorrectionIsNotARecordedBaseRefresh: a generation that only carried the accepted head onto a
// moved base is not a correction. The refresh moves the stand generation to the live one, so the member
// may ride again on the refreshed head, exactly as dag-base-refresh intends.
func TestUnderCorrectionIsNotARecordedBaseRefresh(t *testing.T) {
	q := ucStore(t, "rel-1", 1, 2)
	ctx := context.Background()
	head := "head-refreshed"
	event, revision := "ev-refresh", "rev-refreshed"
	id := RefreshDigest("acc-1", "rel-1", 2, event, revision, head, "owner/repo", "dev", "base-0", "{}", "[]")
	if _, err := q.ExecContext(ctx, "INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?)",
		id, "acc-1", "rel-1", int64(2), event, revision, head, "owner/repo", "dev", "base-0", "{}", "[]", "task-parent", int64(0), "2026-10-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	under, live, stand, err := UnderCorrection(ctx, q, "rel-1")
	if err != nil {
		t.Fatal(err)
	}
	if under || live != 2 || stand != 2 {
		t.Fatalf("UnderCorrection after a base refresh = under %v, live %d, stand %d; want false, 2, 2", under, live, stand)
	}
}

// TestActiveWithLiveGenerationReadsTheAcceptanceAsBefore: the reading is the acceptance
// ActiveForRelationship answered, with the live generation taken in the same statement, and a store
// whose relationships row is absent (a fixture that seeded only the acceptance) answers the
// acceptance's own generation rather than claiming a correction.
func TestActiveWithLiveGenerationReadsTheAcceptanceAsBefore(t *testing.T) {
	q := ucStore(t, "rel-1", 1, 3)
	active, live, found, err := ActiveWithLiveGeneration(context.Background(), q, "rel-1")
	if err != nil || !found {
		t.Fatalf("ActiveWithLiveGeneration = %v %v %v", active, found, err)
	}
	if active.AcceptanceID != "acc-1" || active.EventID != "ev-1" || active.HeadSHA != "head-1" || active.RevisionHash != "rev-1" || active.Generation != 1 {
		t.Fatalf("the acceptance read = %+v", active)
	}
	if live != 3 {
		t.Fatalf("live generation = %d, want 3", live)
	}
	plain, found, err := ActiveForRelationship(context.Background(), q, "rel-1")
	if err != nil || !found {
		t.Fatal(err)
	}
	if plain != active {
		t.Fatalf("the acceptance read apart from the generation = %+v, want %+v", plain, active)
	}
	// an absent relationship row: the acceptance's own generation stands in, so no correction is claimed
	s := openStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-2','p','n','m','rel-orphan',4,'ev-2','rev-2','c','verified','head-2','owner/repo',1,'bound','t','{}','task-parent',0,'2026-10-01T00:00:00Z','active')"); err != nil {
		t.Fatal(err)
	}
	oq := s.Querier(ctx)
	_, live, found, err = ActiveWithLiveGeneration(ctx, oq, "rel-orphan")
	if err != nil || !found {
		t.Fatalf("an acceptance with no relationship row = %v %v", found, err)
	}
	if live != 4 {
		t.Fatalf("live generation without a relationships row = %d, want the acceptance's own 4", live)
	}
	under, _, _, err := UnderCorrection(ctx, oq, "rel-orphan")
	if err != nil || under {
		t.Fatalf("UnderCorrection without a relationships row = %v %v, want false", under, err)
	}
}
