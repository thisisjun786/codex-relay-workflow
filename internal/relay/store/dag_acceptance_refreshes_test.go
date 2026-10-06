package store

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
)

// CRW-728: the two proof tables the DAG zone appends, dag_acceptance_refreshes and dag_verified_heads.
// They are the record dag-accept writes when it accepts a head the parent's ruling verified, and the
// head that ruling verified, so both are append-only ledgers with the constraints of the zone's other
// records: the identity of a refresh is its own content, an acceptance keys its refreshes, an event
// has at most one verified head, and no row is ever updated or deleted.
//
// The statements ship with the same shape as dag_base_refreshes (dag_zone.go): the id carries the
// digest of the row, the pair (acceptance, sequence) and the pair (acceptance, head) are unique, and
// the foreign key points inside the zone only. These tests write the tables directly, as an operator's
// sqlite3 would, so the constraints answer for a writer that skips the store's own insert.

// zoneRefreshRow is one dag_acceptance_refreshes row as SQL, every column named so a reordered
// statement cannot pass by accident.
func zoneRefreshRow(id, acceptance string, seq int, head string) string {
	return fmt.Sprintf("INSERT INTO dag_acceptance_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id,"+
		" execution_generation, event_id, revision_hash, head_sha, verified_head_sha, base_repository, base_ref,"+
		" base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES ('%s','%s',%d,'rel-1',2,'evt','%s','%s','%s','o/r','dev','%s','{}','[]','task-a',0,'2026-10-06T00:00:00Z')",
		id, acceptance, seq, zoneDigest, head, zoneDigest, zoneDigest)
}

// zoneVerifiedHeadRow is one dag_verified_heads row as SQL.
func zoneVerifiedHeadRow(event, head string) string {
	return fmt.Sprintf("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id,"+
		" head_sha, recorded_by_task_id, recorded_at) VALUES ('%s','rel-1',1,'turn-1','%s','task-a','2026-10-06T00:00:00Z')",
		event, head)
}

// The refresh ledger is append-only and keyed by its acceptance: a refresh of an acceptance that does
// not exist is refused (the key points inside the zone), one sequence number holds one row, one head
// holds one row, and no row is updated or deleted.
func TestDAGZoneAcceptanceRefreshesAreAppendOnly(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-1','plan-1','a','%s','rel-1',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-06T00:00:00Z',NULL,'active')", zoneDigest, zoneDigest, zoneDigest))
	zoneRefuses(t, db, "a refresh of an acceptance that does not exist", zoneRefreshRow("r-x", "acc-none", 1, "h1"))
	zoneMustExec(t, db, zoneRefreshRow("r-1", "acc-1", 1, "h1"))
	zoneRefuses(t, db, "a second refresh with the same sequence number", zoneRefreshRow("r-2", "acc-1", 1, "h2"))
	zoneRefuses(t, db, "a second refresh with the same head", zoneRefreshRow("r-3", "acc-1", 2, "h1"))
	zoneRefuses(t, db, "a refresh with an empty id", zoneRefreshRow("", "acc-1", 3, "h3"))
	zoneMustExec(t, db, zoneRefreshRow("r-4", "acc-1", 2, "h2"))
	zoneRefuses(t, db, "an UPDATE of a refresh", "UPDATE dag_acceptance_refreshes SET head_sha = 'h9' WHERE refresh_id = 'r-1'")
	zoneRefuses(t, db, "a DELETE of a refresh", "DELETE FROM dag_acceptance_refreshes")
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM dag_acceptance_refreshes").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("refreshes = %d (%v), want 2", rows, err)
	}
}

// One event has one verified head: a second row for the same event is refused, and no row is updated
// or deleted. The table keys nothing outside the zone, so it is written without any other row.
func TestDAGZoneVerifiedHeadsAreOnePerEvent(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, zoneVerifiedHeadRow("evt-1", "h1"))
	zoneRefuses(t, db, "a second verified head for one event", zoneVerifiedHeadRow("evt-1", "h2"))
	zoneMustExec(t, db, zoneVerifiedHeadRow("evt-2", "h1"))
	zoneRefuses(t, db, "an UPDATE of a verified head", "UPDATE dag_verified_heads SET head_sha = 'h9' WHERE event_id = 'evt-1'")
	zoneRefuses(t, db, "a DELETE of a verified head", "DELETE FROM dag_verified_heads")
	var rows int
	if err := db.QueryRow("SELECT count(*) FROM dag_verified_heads").Scan(&rows); err != nil || rows != 2 {
		t.Fatalf("verified heads = %d (%v), want 2", rows, err)
	}
}

// The refresh and verified-head records are written and read through the store, in the caller's
// transaction: a row written through the store reads back value for value, a row whose id does not
// match the digest of its own content is not read, and a store that predates the tables answers
// absent rather than failing (the tables arrived after the first zone).
func TestDAGAcceptanceRefreshRoundTrips(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	acceptance := "acc-1"
	if _, err := s.DB.ExecContext(ctx, "INSERT INTO dag_acceptances VALUES ('acc-1','plan-1','a','"+zoneDigest+"','rel-1',1,'evt','"+zoneDigest+"','"+zoneDigest+"','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-06T00:00:00Z',NULL,'active')"); err != nil {
		t.Fatal(err)
	}
	row := AcceptanceRefreshRow{
		AcceptanceID: acceptance, RefreshSeq: 1, RelationshipID: "rel-1", ExecutionGeneration: 2, EventID: "evt-2",
		RevisionHash: zoneDigest, HeadSHA: "h1", VerifiedHeadSHA: "h0", BaseRepository: "o/r", BaseRef: "dev",
		BaseTipSHA: zoneDigest, ProofJSON: "{}", ResolvedPathsJSON: "[]",
		RecordedByTaskID: "task-a", CoordinatorEpoch: 0, RecordedAt: "2026-10-06T00:00:00Z",
	}
	row.RefreshID = RefreshDigest(row)
	if err := RecordAcceptanceRefresh(ctx, s, row); err != nil {
		t.Fatal(err)
	}
	read, err := AcceptanceRefresh(ctx, s, acceptance, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, row) {
		t.Fatalf("read back\n %+v\nwant\n %+v", read, row)
	}
	all, err := AcceptanceRefreshes(ctx, s, acceptance)
	if err != nil || len(all) != 1 || !reflect.DeepEqual(all[0], row) {
		t.Fatalf("refreshes = %+v (%v)", all, err)
	}
	// a row written by hand under an id that is not the digest of its content is not read
	hand := row
	hand.RefreshSeq, hand.HeadSHA, hand.RefreshID = 2, "h2", "dar-not-the-digest"
	if err := RecordAcceptanceRefresh(ctx, s, hand); err != nil {
		t.Fatal(err)
	}
	all, err = AcceptanceRefreshes(ctx, s, acceptance)
	if err != nil || len(all) != 1 || all[0].HeadSHA != "h1" {
		t.Fatalf("a row whose id is not its digest was read: %+v (%v)", all, err)
	}
	if _, err := AcceptanceRefresh(ctx, s, acceptance, 2); !errors.Is(err, ErrRefreshNotRecorded) {
		t.Fatalf("a hand-written row read back by sequence: %v", err)
	}
	// a sequence the acceptance never held is absence too, not a database failure
	if _, err := AcceptanceRefresh(ctx, s, acceptance, 7); !errors.Is(err, ErrRefreshNotRecorded) {
		t.Fatalf("a sequence with no row: %v", err)
	}
	// the sequence number is part of the content the id digests: two rows that differ only in it
	// are two identities
	elsewhere := row
	elsewhere.RefreshSeq = 3
	if RefreshDigest(elsewhere) == row.RefreshID {
		t.Fatal("two rows differing only in refresh_seq share one id")
	}
	// a refresh of an acceptance that does not exist is the foreign key's refusal
	missing := row
	missing.AcceptanceID, missing.RefreshSeq, missing.HeadSHA = "acc-none", 1, "h9"
	missing.RefreshID = RefreshDigest(missing)
	if err := RecordAcceptanceRefresh(ctx, s, missing); err == nil {
		t.Fatal("a refresh of an acceptance that does not exist was written")
	}
}

// The verified head of an event round-trips, one event holds one row, and an absent event is not an
// error: it is the answer a store that predates the table gives too.
func TestDAGVerifiedHeadRoundTrips(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	row := VerifiedHeadRow{EventID: "evt-1", RelationshipID: "rel-1", ExecutionGeneration: 1, VerdictTurnID: "turn-1", HeadSHA: "h1", RecordedByTaskID: "task-a", RecordedAt: "2026-10-06T00:00:00Z"}
	if err := RecordVerifiedHead(ctx, s, row); err != nil {
		t.Fatal(err)
	}
	read, found, err := VerifiedHead(ctx, s, "evt-1")
	if err != nil || !found || !reflect.DeepEqual(read, row) {
		t.Fatalf("read back %+v found=%v (%v), want %+v", read, found, err, row)
	}
	if _, found, err := VerifiedHead(ctx, s, "evt-absent"); err != nil || found {
		t.Fatalf("an event with no verified head: found=%v (%v)", found, err)
	}
	second := row
	second.HeadSHA = "h2"
	if err := RecordVerifiedHead(ctx, s, second); err == nil {
		t.Fatal("a second verified head for one event was written")
	}
}

// A store that predates the tables (the shape every existing store has) answers absent from the
// readers instead of failing: the tables arrive with the first write open, and a read-only reader of
// such a store must still answer.
func TestDAGProofReadersOnAStoreWithoutTheZone(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	s, err := OpenReadOnlyStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	if _, err := AcceptanceRefresh(context.Background(), s, "acc-1", 1); !errors.Is(err, ErrRefreshNotRecorded) {
		t.Fatalf("a store without the tables answered %v, want absent", err)
	}
	if rows, err := AcceptanceRefreshes(context.Background(), s, "acc-1"); err != nil || len(rows) != 0 {
		t.Fatalf("a store without the tables answered %+v (%v)", rows, err)
	}
	if _, found, err := VerifiedHead(context.Background(), s, "evt-1"); err != nil || found {
		t.Fatalf("a store without the tables answered found=%v (%v)", found, err)
	}
}
