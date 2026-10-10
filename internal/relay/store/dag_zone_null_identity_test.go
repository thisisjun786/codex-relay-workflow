package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// CRW-835: the DAG zone's identity columns. A SQLite rowid table's TEXT PRIMARY KEY admits NULL, and a
// CHECK on the empty string lets NULL through (merge_trains, CRW-767 post-merge P1). The zone's appended
// statements refuse a NULL identity on insert, and on update where no shipped trigger already refuses
// every update of the row. These tests write the tables directly, as an operator's sqlite3 would, so
// the guards answer for a writer that skips the store's own insert.

// nullKeyGuard is one zone table whose key column the appended statements guard against NULL.
type nullKeyGuard struct {
	table string
	key   string
	// updatable: no shipped trigger refuses every UPDATE of the row, so the guard also has a BEFORE UPDATE OF
	// trigger. Where a shipped trigger already refuses every UPDATE (no_update, immutable), the guard is the insert one only.
	updatable bool
	// emptyRefused: the shipped DDL refuses an empty key with CHECK (<key> <> ''). Nothing appended changes that.
	emptyRefused bool
	// overrides are the columns the table's own shipped BEFORE INSERT trigger reads, set to values that pass it.
	overrides map[string]string
}

func (g nullKeyGuard) insertTrigger() string { return g.table + "_" + g.key + "_not_null" }
func (g nullKeyGuard) updateTrigger() string { return g.insertTrigger() + "_update" }

// zoneNullKeyGuards is every key the appended statements guard: the fifteen tables of CRW-835's list, and
// the two that were checked at build time (dag_user_decisions, delivery_wakes). Their writers take the key
// from a non-empty Go string, and no document reads a NULL key as a value, so the guard refuses nothing a
// product writer writes.
var zoneNullKeyGuards = []nullKeyGuard{
	{table: "dag_plans", key: "plan_id"},
	{table: "dag_input_manifests", key: "manifest_digest", updatable: true},
	{table: "dag_acceptances", key: "acceptance_id", updatable: true},
	{table: "dag_integration_observations", key: "observation_id", updatable: true},
	{table: "dag_decisions", key: "decision_id", updatable: true},
	{table: "dag_merge_checks", key: "check_id", updatable: true},
	{table: "dag_acceptance_revalidations", key: "revalidation_id", updatable: true},
	{table: "dag_acceptance_forge", key: "acceptance_id", updatable: true},
	{table: "dag_conflict_observations", key: "observation_id", updatable: true},
	{table: "dag_summary_outbox", key: "summary_id", emptyRefused: true,
		overrides: map[string]string{"state": "'pending'", "attempts": "0", "seq": "1", "plan_revision": "1"}},
	{table: "dag_tip_conflict_observations", key: "observation_id", updatable: true},
	{table: "dag_base_refreshes", key: "refresh_id", emptyRefused: true},
	{table: "dag_landing_results", key: "result_id", updatable: true},
	{table: "dag_acceptance_refreshes", key: "refresh_id", emptyRefused: true},
	{table: "dag_verified_heads", key: "event_id", emptyRefused: true},
	{table: "dag_user_decisions", key: "decision_id", updatable: true, emptyRefused: true},
	{table: "delivery_wakes", key: "event_id", updatable: true, emptyRefused: true},
}

// zoneFillerDB is a raw connection to path with the CHECK constraints ignored, so a test writes a row
// with placeholder values in the columns it does not model. The NOT NULL columns and the triggers still
// apply: only the CHECKs are skipped, and the key under test is written as given.
func zoneFillerDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := zoneRawDB(t, path)
	if _, err := db.Exec("PRAGMA ignore_check_constraints = ON"); err != nil {
		t.Fatal(err)
	}
	return db
}

// zoneFillerInsert is the INSERT of one row of g's table whose key is keyValue (an SQL literal): every NOT NULL
// column without a default gets a placeholder, and the overrides set what the table's shipped trigger reads.
func zoneFillerInsert(t *testing.T, db *sql.DB, g nullKeyGuard, keyValue string) string {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + g.table + ")")
	if err != nil {
		t.Fatal(err)
	}
	var columns, values []string
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		var value string
		switch {
		case name == g.key:
			value = keyValue
		case g.overrides[name] != "":
			value = g.overrides[name]
		case notnull == 1 && dflt == nil && strings.Contains(strings.ToUpper(typ), "INT"):
			value = "1"
		case notnull == 1 && dflt == nil:
			value = "'x'"
		default:
			continue
		}
		columns = append(columns, name)
		values = append(values, value)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return fmt.Sprintf("INSERT INTO %s (%s) VALUES (%s)", g.table, strings.Join(columns, ","), strings.Join(values, ","))
}

// zoneGuardTriggerNames is the set of triggers the appended statements must add, read from the guard table.
func zoneGuardTriggerNames() []string {
	var names []string
	for _, g := range zoneNullKeyGuards {
		names = append(names, g.insertTrigger())
		if g.updatable {
			names = append(names, g.updateTrigger())
		}
	}
	return names
}

// zoneDropGuards drops the appended guards from a store, to make the store an old zone that predates them.
func zoneDropGuards(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, name := range zoneGuardTriggerNames() {
		if _, err := db.Exec("DROP TRIGGER IF EXISTS " + name); err != nil {
			t.Fatal(err)
		}
	}
}

// Every guard is in the catalog of a store that opened with the zone, under its own name.
func TestDAGZoneNullKeyGuardsAreInTheCatalog(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	catalog := zoneObjects(t, path)
	for _, name := range zoneGuardTriggerNames() {
		if _, ok := catalog["trigger "+name]; !ok {
			t.Errorf("trigger %s is not in the zone", name)
		}
	}
}

// A raw INSERT with a NULL key is refused by the table's guard, for every guarded table.
func TestDAGZoneRefusesANullKeyOnInsert(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneFillerDB(t, path)
	for _, g := range zoneNullKeyGuards {
		t.Run(g.table, func(t *testing.T) {
			statement := zoneFillerInsert(t, db, g, "NULL")
			err := zoneExec(t, db, statement)
			if err == nil || !strings.Contains(err.Error(), "is NULL") {
				t.Fatalf("%s: got %v; want the guard's refusal of a NULL key", statement, err)
			}
		})
	}
	for _, g := range zoneNullKeyGuards {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + g.table).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s holds %d rows after refused NULL inserts (%v)", g.table, n, err)
		}
	}
}

// A raw UPDATE that sets a stored row's key to NULL is refused for every guarded table: by the appended
// BEFORE UPDATE OF guard where no shipped trigger refuses the update, and by the shipped trigger where one
// does (dag_plans, dag_summary_outbox, dag_base_refreshes, dag_acceptance_refreshes, dag_verified_heads).
func TestDAGZoneRefusesANullKeyOnUpdate(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneFillerDB(t, path)
	for _, g := range zoneNullKeyGuards {
		t.Run(g.table, func(t *testing.T) {
			value := "'k-" + g.table + "'"
			zoneMustExec(t, db, zoneFillerInsert(t, db, g, value))
			statement := fmt.Sprintf("UPDATE %s SET %s = NULL WHERE %s = %s", g.table, g.key, g.key, value)
			if err := zoneExec(t, db, statement); err == nil {
				t.Fatalf("%s: a stored key was set to NULL", statement)
			}
			var n int
			if err := db.QueryRow(fmt.Sprintf("SELECT count(*) FROM %s WHERE %s = %s", g.table, g.key, value)).Scan(&n); err != nil || n != 1 {
				t.Fatalf("%s: row with its key after the refused update = %d (%v), want 1", g.table, n, err)
			}
		})
	}
}

// The shipped checks are unchanged: an empty key is refused exactly where the shipped DDL has a CHECK on
// the key being non-empty, and a repeated key is a PRIMARY KEY refusal, which the guards do not touch.
func TestDAGZoneEmptyAndDuplicateKeysKeepTheirShippedBehaviour(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneFillerDB(t, path)
	for _, g := range zoneNullKeyGuards {
		t.Run(g.table, func(t *testing.T) {
			var ddl string
			if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?", g.table).Scan(&ddl); err != nil {
				t.Fatal(err)
			}
			check := "CHECK (" + g.key + " <> '')"
			hasCheck := strings.Contains(strings.Join(strings.Fields(ddl), " "), check)
			if hasCheck != g.emptyRefused {
				t.Fatalf("%s: the shipped DDL has %q = %v, the guard table says %v", g.table, check, hasCheck, g.emptyRefused)
			}
			value := "'k-dup-" + g.table + "'"
			zoneMustExec(t, db, zoneFillerInsert(t, db, g, value))
			if err := zoneExec(t, db, zoneFillerInsert(t, db, g, value)); err == nil {
				t.Fatalf("%s: a repeated key was accepted", g.table)
			} else if strings.Contains(err.Error(), "is NULL") {
				t.Fatalf("%s: a repeated key was refused by the NULL guard: %v", g.table, err)
			}
		})
	}
}

// The foreign key of dag_acceptance_forge is unchanged: a row of a forge names an acceptance that exists, and
// a NULL acceptance is refused by the guard, not by the foreign key.
func TestDAGZoneForgeForeignKeyIsUnchanged(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-1','plan-1','a','%s','rel-1',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-06T00:00:00Z',NULL,'active')", zoneDigest, zoneDigest, zoneDigest))
	zoneMustExec(t, db, "INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES ('acc-1','o/r',7)")
	zoneRefuses(t, db, "a forge of an acceptance that does not exist", "INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES ('acc-none','o/r',8)")
	err := zoneExec(t, db, "INSERT INTO dag_acceptance_forge (acceptance_id, forge_repository, pr_number) VALUES (NULL,'o/r',9)")
	if err == nil || !strings.Contains(err.Error(), "is NULL") {
		t.Fatalf("a forge with a NULL acceptance: %v; want the guard's refusal", err)
	}
}

// A store that predates the guards (an old zone with NULL-key rows already in it) upgrades: the guards are
// added, every row it holds stays as it was, and a new NULL key is refused. The old zone is made here by
// dropping the guards from a store the current build created, which leaves every shipped object as it was.
func TestDAGZoneUpgradeKeepsNullKeyRows(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	old := zoneFillerDB(t, path)
	zoneDropGuards(t, old)
	for _, g := range zoneNullKeyGuards {
		zoneMustExec(t, old, zoneFillerInsert(t, old, g, "NULL"))
	}
	before := map[string]int{}
	for _, g := range zoneNullKeyGuards {
		var n int
		if err := old.QueryRow("SELECT count(*) FROM " + g.table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		before[g.table] = n
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	zoneOpenClose(t, path)
	catalog := zoneObjects(t, path)
	for _, name := range zoneGuardTriggerNames() {
		if _, ok := catalog["trigger "+name]; !ok {
			t.Errorf("trigger %s was not added on upgrade", name)
		}
	}
	db := zoneFillerDB(t, path)
	for _, g := range zoneNullKeyGuards {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + g.table).Scan(&n); err != nil || n != before[g.table] {
			t.Errorf("%s: %d rows after upgrade, want %d (%v)", g.table, n, before[g.table], err)
		}
	}
	for _, g := range zoneNullKeyGuards {
		if err := zoneExec(t, db, zoneFillerInsert(t, db, g, "NULL")); err == nil {
			t.Errorf("%s: a NULL key was accepted after the upgrade", g.table)
		}
	}
}

// The readers of dag_acceptance_refreshes skip a row whose id is NULL, a row a store written before the
// guard may hold: the valid refresh is still read, and the NULL row does not fail the read.
func TestDAGAcceptanceRefreshReadersSkipANullKeyRow(t *testing.T) {
	t.Parallel()
	s := recordStore(t)
	ctx := context.Background()
	if _, err := s.DB.ExecContext(ctx, fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-1','plan-1','a','%s','rel-1',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-06T00:00:00Z',NULL,'active')", zoneDigest, zoneDigest, zoneDigest)); err != nil {
		t.Fatal(err)
	}
	// an old store's NULL-id refresh: the guard is dropped to write it, as a store that predates the guard holds it
	if _, err := s.DB.ExecContext(ctx, "DROP TRIGGER IF EXISTS dag_acceptance_refreshes_refresh_id_not_null"); err != nil {
		t.Fatal(err)
	}
	nullRow := strings.Replace(zoneRefreshRow("x", "acc-1", 5, "h5"), "VALUES ('x'", "VALUES (NULL", 1)
	if _, err := s.DB.ExecContext(ctx, nullRow); err != nil {
		t.Fatalf("%s: %v", nullRow, err)
	}
	row := AcceptanceRefreshRow{
		AcceptanceID: "acc-1", RefreshSeq: 1, RelationshipID: "rel-1", ExecutionGeneration: 2, EventID: "evt-2",
		RevisionHash: zoneDigest, HeadSHA: "h1", VerifiedHeadSHA: "h0", BaseRepository: "o/r", BaseRef: "dev",
		BaseTipSHA: zoneDigest, ProofJSON: "{}", ResolvedPathsJSON: "[]",
		RecordedByTaskID: "task-a", CoordinatorEpoch: 0, RecordedAt: "2026-10-06T00:00:00Z",
	}
	row.RefreshID = RefreshDigest(row)
	if err := RecordAcceptanceRefresh(ctx, s, row); err != nil {
		t.Fatal(err)
	}
	read, err := AcceptanceRefresh(ctx, s, "acc-1", 1)
	if err != nil || read != row {
		t.Fatalf("AcceptanceRefresh(seq 1) = %+v (%v), want the valid row", read, err)
	}
	if _, err := AcceptanceRefresh(ctx, s, "acc-1", 5); !errors.Is(err, ErrRefreshNotRecorded) {
		t.Fatalf("AcceptanceRefresh(seq 5) = %v, want ErrRefreshNotRecorded for the NULL-id row", err)
	}
	all, err := AcceptanceRefreshes(ctx, s, "acc-1")
	if err != nil || len(all) != 1 || all[0] != row {
		t.Fatalf("AcceptanceRefreshes = %+v (%v), want only the valid row", all, err)
	}
}
