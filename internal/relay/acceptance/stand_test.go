package acceptance

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-768 generation 2: the stand reading moved here so the DAG scheduler and the merge train apply one
// rule. These tests pin the digest spelling and the two readings against a temporary store.

func openStore(t *testing.T) *store.Store {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestRefreshDigestSpelling pins the dbr id: the spelling existing stores hold.
func TestRefreshDigestSpelling(t *testing.T) {
	got := RefreshDigest("acc-1", "rel-1", 2, "ev-1", "rev-1", "head-1", "owner/repo", "dev", "base-1", "{}", "[]")
	if len(got) != len("dbr-")+32 || got[:4] != "dbr-" {
		t.Fatalf("refresh digest = %q, want a dbr- id of 32 hex digits", got)
	}
	// the same content digests alike and different content does not
	if got != RefreshDigest("acc-1", "rel-1", 2, "ev-1", "rev-1", "head-1", "owner/repo", "dev", "base-1", "{}", "[]") {
		t.Fatal("the same refresh digests differently")
	}
	if got == RefreshDigest("acc-1", "rel-1", 2, "ev-1", "rev-1", "head-2", "owner/repo", "dev", "base-1", "{}", "[]") {
		t.Fatal("a different head digests alike")
	}
}

// TestStandOfIsOwnWithoutARefresh: with no recorded refresh an acceptance stands on itself.
func TestStandOfIsOwnWithoutARefresh(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	stand, err := StandOf(ctx, s.Querier(ctx), "acc-1", "rel-1", 1, "ev-1", "rev-1", "head-1")
	if err != nil {
		t.Fatal(err)
	}
	want := Stand{RelationshipID: "rel-1", Generation: 1, EventID: "ev-1", RevisionHash: "rev-1", Head: "head-1"}
	if stand != want {
		t.Fatalf("stand = %+v, want %+v", stand, want)
	}
}

// TestStandOfReadsTheNewestValidRefresh: a recorded refresh moves the stand; a row that does not digest
// to its id, and one of another generation, are ignored.
func TestStandOfReadsTheNewestValidRefresh(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	q := s.Querier(ctx)
	// dag_base_refreshes references dag_acceptances, so the acceptance row comes first
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-1','p','n','m','rel-1',1,'ev-1','rev-1','c','verified','head-1','owner/repo',1,'bound','t','{}','task-a',0,'2026-10-01T00:00:00Z','active')"); err != nil {
		t.Fatal(err)
	}
	seq := int64(0)
	insert := func(id, relationship string, generation int64, head string) {
		t.Helper()
		seq++
		_, err := s.DB.ExecContext(ctx, "INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
			" VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)",
			id, "acc-1", seq, relationship, generation, "ev-"+head, "rev-"+head, head, "owner/repo", "dev", "base-1", "{}", "[]", "task-a", int64(0), "2026-10-01T00:00:00Z")
		if err != nil {
			t.Fatal(err)
		}
	}
	// a row that does not digest to its id is ignored
	insert("dbr-not-the-digest", "rel-1", 2, "head-bogus")
	stand, err := StandOf(ctx, q, "acc-1", "rel-1", 1, "ev-1", "rev-1", "head-1")
	if err != nil {
		t.Fatal(err)
	}
	if stand.Head != "head-1" {
		t.Fatalf("an invalid row moved the stand to %q", stand.Head)
	}
	// a valid row moves it
	good := RefreshDigest("acc-1", "rel-1", 2, "ev-head-2", "rev-head-2", "head-2", "owner/repo", "dev", "base-1", "{}", "[]")
	insert(good, "rel-1", 2, "head-2")
	stand, err = StandOf(ctx, q, "acc-1", "rel-1", 1, "ev-1", "rev-1", "head-1")
	if err != nil {
		t.Fatal(err)
	}
	if stand.Head != "head-2" || stand.RefreshID != good || stand.Generation != 2 {
		t.Fatalf("stand = %+v, want the recorded refresh head", stand)
	}
	// a row of another relationship is ignored
	other := RefreshDigest("acc-1", "rel-2", 3, "ev-head-3", "rev-head-3", "head-3", "owner/repo", "dev", "base-1", "{}", "[]")
	insert(other, "rel-2", 3, "head-3")
	stand, err = StandOf(ctx, q, "acc-1", "rel-1", 1, "ev-1", "rev-1", "head-1")
	if err != nil {
		t.Fatal(err)
	}
	if stand.Head != "head-2" {
		t.Fatalf("another relationship's refresh moved the stand to %q", stand.Head)
	}
}

// TestActiveForRelationship: the newest active acceptance of a relationship, and none for another.
func TestActiveForRelationship(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	q := s.Querier(ctx)
	if _, found, err := ActiveForRelationship(ctx, q, "rel-none"); err != nil || found {
		t.Fatalf("a relationship with no acceptance: found=%v (%v)", found, err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_acceptances (acceptance_id, plan_id, node_id, manifest_digest, relationship_id, execution_generation, event_id, revision_hash, criteria_set_digest, verdict, head_sha, repository, pr_number, ack_tier, verdict_turn_id, rule_version_json, accepted_by_task_id, coordinator_epoch, accepted_at, state)"+
		" VALUES ('acc-1','p','n','m','rel-1',1,'ev-1','rev-1','c','verified','head-1','owner/repo',1,'bound','t','{}','task-a',0,'2026-10-01T00:00:00Z','active')"); err != nil {
		t.Fatal(err)
	}
	active, found, err := ActiveForRelationship(ctx, q, "rel-1")
	if err != nil || !found {
		t.Fatalf("found=%v (%v)", found, err)
	}
	if active.AcceptanceID != "acc-1" || active.EventID != "ev-1" || active.HeadSHA != "head-1" || active.Generation != 1 {
		t.Fatalf("active = %+v", active)
	}
	// a superseded acceptance is not active
	if _, err := s.DB.ExecContext(ctx, "UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = 'acc-1'"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := ActiveForRelationship(ctx, q, "rel-1"); err != nil || found {
		t.Fatalf("a superseded acceptance read as active: found=%v (%v)", found, err)
	}
}

// TestRulingHead: dag_verified_heads wins, and the acceptance's own head is the fallback.
func TestRulingHead(t *testing.T) {
	s := openStore(t)
	ctx := context.Background()
	q := s.Querier(ctx)
	head, source, err := RulingHead(ctx, q, "ev-1", "head-1")
	if err != nil || head != "head-1" || source != "acceptance" {
		t.Fatalf("no row: head=%q source=%q (%v)", head, source, err)
	}
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at)"+
		" VALUES ('ev-1','rel-1',1,'turn-1','head-verified','task-a','2026-10-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	head, source, err = RulingHead(ctx, q, "ev-1", "head-1")
	if err != nil || head != "head-verified" || source != "verified_heads" {
		t.Fatalf("with a row: head=%q source=%q (%v)", head, source, err)
	}
}
