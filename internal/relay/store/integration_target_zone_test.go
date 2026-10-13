package store

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// CRW-1304: the integration target record (dag_integration_targets), appended to the zone ledger after the agent acceptance tables. A row says which remote branch an integration ref is
// pushed to (kind target) or which head that remote branch held when the relay last read it (kind observed). The table is append-only, refuses a NULL key (CRW-835) and a REPLACE of a
// stored key or rowid (CRW-974, the generic key-guard tests take its key), and a zone that lacks it is extended by the next write open with every row kept. The first tests write the table
// directly, as an operator's sqlite3 would, on a store the build opened (foreign keys on); the later ones go through the typed writers.

// targetTables are the tables CRW-1304 appends to the zone.
var targetTables = []string{"dag_integration_targets"}

// targetRowSQL is the INSERT of one row; every value is an SQL literal.
func targetRowSQL(id, kind, head string) string {
	return fmt.Sprintf("INSERT INTO dag_integration_targets (target_id, kind, checkout, integration_ref, remote, remote_ref, plan_id, observed_head, source, recorded_by, recorded_at)"+
		" VALUES (%s,%s,'/work/repo','dev-int','origin','dev','g',%s,'record','parent','2026-10-14T00:00:00Z')", id, kind, head)
}

// A row of the record is never edited or removed, a target row carries no observed head and an observed row carries one, and the identity columns are never empty.
func TestIntegrationTargetZoneGuards(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	zoneMustExec(t, db, targetRowSQL("'t1'", "'target'", "''"), targetRowSQL("'t2'", "'observed'", "'0123456789012345678901234567890123456789'"))
	for _, table := range targetTables {
		before := zoneRows(t, db, table)
		if len(before) == 0 {
			t.Fatalf("%s holds no row", table)
		}
		if err := zoneExec(t, db, "UPDATE "+table+" SET rowid = rowid"); err == nil || !strings.Contains(err.Error(), table+" rows are append-only: never updated") {
			t.Errorf("%s: UPDATE: %v", table, err)
		}
		if err := zoneExec(t, db, "DELETE FROM "+table); err == nil || !strings.Contains(err.Error(), table+" rows are append-only: never deleted") {
			t.Errorf("%s: DELETE: %v", table, err)
		}
		if after := zoneRows(t, db, table); !slices.Equal(before, after) {
			t.Errorf("%s changed over refused edits", table)
		}
	}
	zoneRefuses(t, db, "a target row that names an observed head", targetRowSQL("'x1'", "'target'", "'0123456789012345678901234567890123456789'"))
	zoneRefuses(t, db, "an observed row with no head", targetRowSQL("'x2'", "'observed'", "''"))
	zoneRefuses(t, db, "an unknown kind", targetRowSQL("'x3'", "'moved'", "''"))
	zoneRefuses(t, db, "an empty id", targetRowSQL("''", "'target'", "''"))
	zoneRefuses(t, db, "an empty checkout", strings.Replace(targetRowSQL("'x4'", "'target'", "''"), "'/work/repo'", "''", 1))
	zoneRefuses(t, db, "an empty remote ref", strings.Replace(targetRowSQL("'x5'", "'target'", "''"), "'origin','dev'", "'origin',''", 1))
	zoneRefuses(t, db, "an unknown source", strings.Replace(targetRowSQL("'x6'", "'target'", "''"), "'record'", "'hand'", 1))
	zoneRefuses(t, db, "an empty actor", strings.Replace(targetRowSQL("'x7'", "'target'", "''"), "'parent'", "''", 1))
	if rows := zoneRows(t, db, "dag_integration_targets"); len(rows) != 2 {
		t.Errorf("dag_integration_targets holds %d rows after the refusals, want the 2 that were accepted: %q", len(rows), rows)
	}
}

// The key column refuses NULL (CRW-835), and a REPLACE of a stored key or rowid is refused with the text of a plain INSERT (CRW-974): the row stays as it was.
func TestIntegrationTargetZoneNullAndNoReplaceGuards(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	if err := zoneExec(t, db, targetRowSQL("NULL", "'target'", "''")); err == nil || !strings.Contains(err.Error(), "dag_integration_targets.target_id is NULL: a row is addressed by a non-empty id") {
		t.Errorf("NULL id: %v", err)
	}
	zoneMustExec(t, db, targetRowSQL("'t1'", "'target'", "''"))
	before := zoneRows(t, db, "dag_integration_targets")
	for _, verb := range []string{"INSERT OR REPLACE INTO", "INSERT INTO", "REPLACE INTO"} {
		statement := strings.Replace(targetRowSQL("'t1'", "'target'", "''"), "INSERT INTO", verb, 1)
		if err := zoneExec(t, db, statement); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_integration_targets.target_id") {
			t.Errorf("%s: %v; want the refusal of the target id", verb, err)
		}
	}
	var rowid int64
	if err := db.QueryRow("SELECT rowid FROM dag_integration_targets").Scan(&rowid); err != nil {
		t.Fatal(err)
	}
	replace := strings.Replace(targetRowSQL("'t-free'", "'target'", "''"), "INSERT INTO dag_integration_targets (", "INSERT OR REPLACE INTO dag_integration_targets (rowid, ", 1)
	replace = strings.Replace(replace, "VALUES (", fmt.Sprintf("VALUES (%d, ", rowid), 1)
	if err := zoneExec(t, db, replace); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_integration_targets.rowid") {
		t.Errorf("REPLACE of the stored rowid: %v", err)
	}
	if after := zoneRows(t, db, "dag_integration_targets"); !slices.Equal(before, after) {
		t.Errorf("the table changed over refused REPLACEs:\n%q\n%q", before, after)
	}
}

// A zone that ends before the record is extended by the open that finds the table missing, with every row it held kept and the text of a store the build created.
func TestIntegrationTargetZoneUpgradeFromOldZone(t *testing.T) {
	t.Parallel()
	upgradeCheck(t, targetTables)
}

const targetHeadA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const targetHeadB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func targetStore(t *testing.T) *Store {
	t.Helper()
	s, err := fixtureOpen(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func targetOf(remote, remoteRef string) IntegrationTargetRow {
	return IntegrationTargetRow{Kind: TargetKindTarget, Checkout: "/work/repo", IntegrationRef: "dev-int", Remote: remote, RemoteRef: remoteRef, PlanID: "g", Source: TargetSourceRecord, RecordedBy: "parent", RecordedAt: "2026-10-14T00:00:00Z"}
}

func observedOf(target IntegrationTargetRow, head string) IntegrationTargetRow {
	target.Kind, target.ObservedHead, target.Source = TargetKindObserved, head, TargetSourceCheck
	return target
}

// A repeated identical record is a no-op and a changed one appends; the current target of an integration ref is its newest target row. An observed head is a no-op while it is the newest
// one of its target and appends when it differs, also when it returns to an earlier head.
func TestRecordIntegrationTargetAppendsOnlyChanges(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := targetStore(t)
	first := targetOf("origin", "dev")
	if appended, err := RecordIntegrationTarget(ctx, s, first); err != nil || !appended {
		t.Fatalf("first record: %v, %v; want appended", appended, err)
	}
	again := first
	again.RecordedBy, again.RecordedAt, again.PlanID = "someone-else", "2026-10-14T01:00:00Z", "other-plan"
	if appended, err := RecordIntegrationTarget(ctx, s, again); err != nil || appended {
		t.Fatalf("the same target again: %v, %v; want a no-op", appended, err)
	}
	changed := targetOf("origin", "main")
	if appended, err := RecordIntegrationTarget(ctx, s, changed); err != nil || !appended {
		t.Fatalf("a changed remote ref: %v, %v; want appended", appended, err)
	}
	other := targetOf("origin", "dev")
	other.IntegrationRef = "other-int"
	if appended, err := RecordIntegrationTarget(ctx, s, other); err != nil || !appended {
		t.Fatalf("another integration ref: %v, %v; want appended", appended, err)
	}
	current, err := IntegrationTargets(ctx, s)
	if err != nil || len(current) != 2 {
		t.Fatalf("current targets: %+v, %v; want one per integration ref", current, err)
	}
	if current[0].IntegrationRef != "dev-int" || current[0].RemoteRef != "main" || current[1].IntegrationRef != "other-int" || current[1].RemoteRef != "dev" {
		t.Fatalf("current targets %+v; want dev-int on main and other-int on dev", current)
	}
	// going back to an earlier target is a change: the newest row decides, not the history
	if appended, err := RecordIntegrationTarget(ctx, s, first); err != nil || !appended {
		t.Fatalf("back to the first target: %v, %v; want appended", appended, err)
	}
	if current, _ = IntegrationTargets(ctx, s); current[0].RemoteRef != "dev" {
		t.Fatalf("current target of dev-int %+v; want dev again", current[0])
	}
	if known, err := IntegrationTargetRecorded(ctx, s, "/work/repo", "dev-int", "origin", "main"); err != nil || !known {
		t.Fatalf("a target that was recorded once: %v, %v", known, err)
	}
	if known, err := IntegrationTargetRecorded(ctx, s, "/work/repo", "dev-int", "origin", "other"); err != nil || known {
		t.Fatalf("a target that was never recorded: %v, %v", known, err)
	}

	target := current[0]
	if _, found, err := LatestIntegrationObservation(ctx, s, target); err != nil || found {
		t.Fatalf("an observation before any: %v, %v", found, err)
	}
	for i, head := range []string{targetHeadA, targetHeadA, targetHeadB, targetHeadA} {
		appended, err := RecordIntegrationTarget(ctx, s, observedOf(target, head))
		if want := i != 1; err != nil || appended != want {
			t.Fatalf("observation %d of %s: appended %v, %v; want %v", i, head, appended, err, want)
		}
	}
	latest, found, err := LatestIntegrationObservation(ctx, s, target)
	if err != nil || !found || latest.ObservedHead != targetHeadA {
		t.Fatalf("latest observation %+v, %v, %v; want %s", latest, found, err, targetHeadA)
	}
	if _, found, _ := LatestIntegrationObservation(ctx, s, targetOf("origin", "main")); found {
		t.Fatal("an observation of the dev target is not an observation of the main target")
	}
	rows, err := queryRows(ctx, s, scanIntegrationTarget, "SELECT "+integrationTargetColumns+" FROM dag_integration_targets ORDER BY rowid")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, r := range rows {
		ids[r.TargetID] = true
	}
	if len(rows) != 7 || len(ids) != len(rows) {
		t.Fatalf("%d rows with %d distinct ids; want 7 distinct rows", len(rows), len(ids))
	}
}

// A kind or a source the table does not know, and an empty identity, are refused before a row is written.
func TestRecordIntegrationTargetRefusesMalformedRows(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := targetStore(t)
	for name, mutate := range map[string]func(*IntegrationTargetRow){
		"empty checkout":     func(r *IntegrationTargetRow) { r.Checkout = "" },
		"empty remote":       func(r *IntegrationTargetRow) { r.Remote = "" },
		"unknown source":     func(r *IntegrationTargetRow) { r.Source = "hand" },
		"target with a head": func(r *IntegrationTargetRow) { r.ObservedHead = targetHeadA },
		"empty actor":        func(r *IntegrationTargetRow) { r.RecordedBy = "" },
	} {
		row := targetOf("origin", "dev")
		mutate(&row)
		if appended, err := RecordIntegrationTarget(ctx, s, row); err == nil || appended {
			t.Errorf("%s: %v, %v; want a refusal", name, appended, err)
		}
	}
	if current, err := IntegrationTargets(ctx, s); err != nil || len(current) != 0 {
		t.Fatalf("refused rows left %+v, %v", current, err)
	}
}
