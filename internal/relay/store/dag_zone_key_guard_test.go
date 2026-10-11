package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"modernc.org/sqlite"
)

// CRW-974: the append-only zone tables refuse a row that would REPLACE another.
//
// UPDATE and DELETE of these tables are refused by triggers (<table>_no_update, <table>_no_delete). A
// conflicting INSERT OR REPLACE deletes the row it replaces without running a DELETE trigger while
// recursive_triggers is OFF (the store's connection setting, unchanged), so the table could be
// rewritten by a writer that skips the store's own insert. The appended statements add, for every key
// of every table that has a <table>_no_delete trigger (the primary key, each UNIQUE and each partial
// UNIQUE index), a BEFORE INSERT guard that raises the text SQLite gives a plain INSERT of that key.
// These tests write the tables directly, as an operator's sqlite3 would, on a raw connection with
// recursive_triggers=OFF.

// keyGuard is one key of an append-only zone table and the guard the appended statements hold for it. It
// is the template a later append-only table reuses (zoneKeyGuardSQL): name, key columns, the WHERE of a
// partial index (where, and newWhere for the same condition on the incoming row), and earlier, the WHEN
// of a trigger the table already has that fires BEFORE INSERT with a message of its own.
type keyGuard struct {
	table    string
	suffix   string
	cols     []string
	where    string
	newWhere string
	// whereCols are the columns the partial key's WHERE reads: a test's rows carry them so a row is in (or out of) the index.
	whereCols []string
	earlier   string
	// probe lists columns beyond the key a test's conflicting row shares with the stored row so that the table's
	// earlier trigger lets it through (an outbox entry repeating a summary_id stays in its stream).
	probe []string
}

func (g keyGuard) name() string { return g.table + "_" + g.suffix + "_guard" }

// message is the text SQLite gives a plain INSERT that repeats this key.
func (g keyGuard) message() string {
	var parts []string
	for _, c := range g.cols {
		parts = append(parts, g.table+"."+c)
	}
	return "UNIQUE constraint failed: " + strings.Join(parts, ", ")
}

// zoneKeyGuardSQL is the statement a key's guard is: refuse the incoming row when a stored row holds the
// same key (a partial key only when both rows are in the index), unless the table's own earlier
// BEFORE INSERT trigger already refuses the row with its message (that trigger was created before the
// guard, and triggers fire last-created-first, so the guard stands aside to leave its message as it was).
// SQLite fires triggers in reverse order of creation and checks unique indexes last-declared-first with
// the primary key last, so a table's guards are appended primary key first, then the UNIQUEs in the order
// of the table's DDL, then its partial indexes in the order they were created.
func zoneKeyGuardSQL(g keyGuard) string {
	var match []string
	for _, c := range g.cols {
		match = append(match, c+" = NEW."+c)
	}
	inner := strings.Join(match, " AND ")
	var when []string
	if g.where != "" {
		inner += " AND " + g.where
		when = append(when, g.newWhere)
	}
	if g.earlier != "" {
		when = append(when, "NOT COALESCE(("+g.earlier+"), 0)")
	}
	when = append(when, "EXISTS (SELECT 1 FROM "+g.table+" WHERE "+inner+")")
	return "CREATE TRIGGER IF NOT EXISTS " + g.name() + " BEFORE INSERT ON " + g.table + "\nWHEN " + strings.Join(when, " AND ") +
		"\nBEGIN SELECT RAISE(ABORT, '" + g.message() + "'); END"
}

// zoneRowidMarksSQL is the table the rowid -1 guards keep their marks in: one row per guarded table that held a row under
// rowid -1 when an insert that reads -1 began, none in a store whose tables hold no rowid -1.
const zoneRowidMarksSQL = "CREATE TABLE IF NOT EXISTS dag_zone_rowid_marks (\n  table_name TEXT NOT NULL\n)"

// zoneRowidGuardSQL are the three statements a rowid table gets for its implicit key: the rowid. An INSERT OR REPLACE
// that names the rowid of a stored row (rowid, _rowid_ or oid) replaces it whatever the declared keys say.
// before refuses the row when a stored row holds NEW.rowid, with the text a plain INSERT of it gets; it is appended
// after the table's key guards so it fires first, as SQLite checks the rowid before the declared keys, and it stands
// aside under the same earlier-trigger condition as the key guards. A BEFORE INSERT trigger reads -1 as NEW.rowid
// for every row whose rowid SQLite assigns afterwards, so before stands aside for -1. A row may be stored under -1
// (SQLite assigns it when the largest rowid is -2), so mark (BEFORE INSERT, WHEN NEW.rowid = -1) records in
// dag_zone_rowid_marks whether a stored row has rowid -1 as the incoming row arrives, and after (AFTER INSERT) refuses
// the row that landed under -1 when one did: an assigned rowid is then the next above the largest, so only a REPLACE of
// the stored row puts the new one there. The statement is rolled back, the REPLACE's deletion included, and an
// assigned insert is never refused.
func zoneRowidGuardSQL(table, earlier string) (before, mark, after string) {
	when := "NEW.rowid <> -1"
	if earlier != "" {
		when += " AND NOT COALESCE((" + earlier + "), 0)"
	}
	before = "CREATE TRIGGER IF NOT EXISTS " + table + "_rowid_guard BEFORE INSERT ON " + table + "\nWHEN " + when +
		" AND EXISTS (SELECT 1 FROM " + table + " WHERE rowid = NEW.rowid)\nBEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: " + table + ".rowid'); END"
	mark = "CREATE TRIGGER IF NOT EXISTS " + table + "_rowid_minus1_mark_guard BEFORE INSERT ON " + table + "\nWHEN NEW.rowid = -1\n" +
		"BEGIN DELETE FROM dag_zone_rowid_marks WHERE table_name = '" + table + "';\n" +
		"  INSERT INTO dag_zone_rowid_marks (table_name) SELECT '" + table + "' WHERE EXISTS (SELECT 1 FROM " + table + " WHERE rowid = -1); END"
	after = "CREATE TRIGGER IF NOT EXISTS " + table + "_rowid_minus1_guard AFTER INSERT ON " + table +
		"\nWHEN NEW.rowid = -1 AND EXISTS (SELECT 1 FROM dag_zone_rowid_marks WHERE table_name = '" + table + "')\n" +
		"BEGIN SELECT RAISE(ABORT, 'UNIQUE constraint failed: " + table + ".rowid'); END"
	return before, mark, after
}

// zoneRowidEarlier is the earlier BEFORE INSERT trigger's condition of the tables that have one.
func zoneRowidEarlier(table string) string {
	for _, g := range zoneKeyGuardsOf(table) {
		if g.earlier != "" {
			return g.earlier
		}
	}
	return ""
}

// zoneRowidGuardNames are the names of the rowid guards of every guarded table.
func zoneRowidGuardNames() []string {
	var names []string
	for _, table := range zoneKeyGuardTables() {
		names = append(names, table+"_rowid_guard", table+"_rowid_minus1_mark_guard", table+"_rowid_minus1_guard")
	}
	return names
}

const (
	outboxEarlier = `NEW.state <> 'pending' OR NEW.attempts <> 0
    OR NEW.seq <> COALESCE((SELECT MAX(seq) FROM dag_summary_outbox WHERE plan_id = NEW.plan_id AND document = NEW.document), 0) + 1
    OR NEW.plan_revision < COALESCE((SELECT MAX(plan_revision) FROM dag_summary_outbox WHERE plan_id = NEW.plan_id AND document = NEW.document), 0)`
	revisionEarlier = `NEW.parent_revision_no > 0 AND NOT EXISTS (
      SELECT 1 FROM dag_plan_revisions WHERE plan_id = NEW.plan_id AND revision_no = NEW.parent_revision_no)`
)

// zoneKeyGuards is every key of every table that has a <table>_no_delete trigger (the 17 tables of the
// append-only zone), in the order the guards are appended.
var zoneKeyGuards = []keyGuard{
	{table: "dag_plans", suffix: "pk", cols: []string{"plan_id"}},
	{table: "dag_plan_revisions", suffix: "pk", cols: []string{"plan_id", "revision_no"}, earlier: revisionEarlier},
	{table: "dag_plan_revisions", suffix: "request_key", cols: []string{"plan_id", "request_id"}, earlier: revisionEarlier},
	{table: "dag_plan_revisions", suffix: "parent_key", cols: []string{"plan_id", "parent_revision_no"}, earlier: revisionEarlier},
	{table: "dag_nodes", suffix: "pk", cols: []string{"plan_id", "node_id", "introduced_rev"}},
	{table: "dag_nodes", suffix: "live_key", cols: []string{"plan_id", "node_id"}, where: "retired_rev IS NULL", newWhere: "NEW.retired_rev IS NULL", whereCols: []string{"retired_rev"}},
	{table: "dag_edges", suffix: "pk", cols: []string{"plan_id", "edge_id"}},
	{table: "dag_summary_outbox", suffix: "pk", cols: []string{"summary_id"}, earlier: outboxEarlier, probe: []string{"plan_id", "document"}},
	{table: "dag_summary_outbox", suffix: "open_key", cols: []string{"plan_id", "document"}, earlier: outboxEarlier,
		where: "state IN ('pending', 'claimed', 'failed')", newWhere: "NEW.state IN ('pending', 'claimed', 'failed')", whereCols: []string{"state"}},
	{table: "dag_base_refreshes", suffix: "pk", cols: []string{"refresh_id"}},
	{table: "dag_base_refreshes", suffix: "seq_key", cols: []string{"acceptance_id", "refresh_seq"}},
	{table: "dag_base_refreshes", suffix: "head_key", cols: []string{"acceptance_id", "event_id", "head_sha"}},
	{table: "dag_generation_withdrawals", suffix: "pk", cols: []string{"relationship_id", "execution_generation"}},
	{table: "dag_acceptance_refreshes", suffix: "pk", cols: []string{"refresh_id"}},
	{table: "dag_acceptance_refreshes", suffix: "seq_key", cols: []string{"acceptance_id", "refresh_seq"}},
	{table: "dag_acceptance_refreshes", suffix: "head_key", cols: []string{"acceptance_id", "head_sha"}},
	{table: "dag_verified_heads", suffix: "pk", cols: []string{"event_id"}},
	{table: "merge_trains", suffix: "pk", cols: []string{"train_id"}},
	{table: "merge_train_members", suffix: "pk", cols: []string{"train_id", "seq"}},
	{table: "merge_train_events", suffix: "pk", cols: []string{"train_id", "seq"}},
	{table: "dag_acceptance_verifications", suffix: "pk", cols: []string{"acceptance_id"}},
	{table: "dag_integration_batches", suffix: "pk", cols: []string{"batch_id"}},
	{table: "dag_integration_stages", suffix: "pk", cols: []string{"stage_id"}},
	{table: "dag_acceptance_premerge", suffix: "pk", cols: []string{"acceptance_id"}},
	{table: "dag_revalidation_premerge", suffix: "pk", cols: []string{"revalidation_id"}},
	// CRW-1212: the flow journal and the agent execution rows. dag_agent_executions has two partial keys (a chain has one root per node, and a row is the predecessor of at most one row).
	{table: "flow_runs", suffix: "pk", cols: []string{"run_id"}},
	{table: "flow_calls", suffix: "pk", cols: []string{"call_id", "event_seq"}},
	{table: "flow_events", suffix: "pk", cols: []string{"run_id", "seq"}},
	{table: "flow_spawn_decisions", suffix: "pk", cols: []string{"decision_id"}},
	{table: "flow_step_decisions", suffix: "pk", cols: []string{"step_id"}},
	{table: "dag_agent_executions", suffix: "pk", cols: []string{"execution_id"}},
	{table: "dag_agent_executions", suffix: "root_key", cols: []string{"plan_id", "node_id"}, where: "supersedes_execution_id IS NULL", newWhere: "NEW.supersedes_execution_id IS NULL", whereCols: []string{"supersedes_execution_id"}},
	{table: "dag_agent_executions", suffix: "successor_key", cols: []string{"supersedes_execution_id"}, where: "supersedes_execution_id IS NOT NULL", newWhere: "NEW.supersedes_execution_id IS NOT NULL", whereCols: []string{"supersedes_execution_id"}},
	{table: "dag_agent_closures", suffix: "pk", cols: []string{"subject_kind", "subject_id"}},
}

// zoneKeyGuardExempt are the unique keys of the guarded tables that have no guard, with the reason. The unique
// key (plan_id, document, seq) of dag_summary_outbox cannot be repeated by any INSERT, REPLACE included:
// dag_summary_outbox_order (BEFORE INSERT, created before any guard) refuses every entry whose seq is not
// MAX(seq) + 1 of its stream, and a repeated seq is never that, so the key is already unreachable. A guard
// for it would have the order trigger's WHEN as its stand-aside condition and could never fire.
var zoneKeyGuardExempt = map[string]map[string]string{
	"dag_summary_outbox": {"plan_id,document,seq": "dag_summary_outbox_order refuses every seq that is not the next of its stream"},
}

func zoneKeyGuardTables() []string {
	var tables []string
	for _, g := range zoneKeyGuards {
		if !slices.Contains(tables, g.table) {
			tables = append(tables, g.table)
		}
	}
	return tables
}

func zoneKeyGuardsOf(table string) []keyGuard {
	var out []keyGuard
	for _, g := range zoneKeyGuards {
		if g.table == table {
			out = append(out, g)
		}
	}
	return out
}

// zoneKeyDB is a raw connection with recursive_triggers OFF (read back, not assumed) and the CHECK
// constraints ignored so a test writes placeholder values in the columns it does not model. The
// triggers, the NOT NULL columns and the unique keys all apply.
func zoneKeyDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db := zoneRawDB(t, path)
	zoneMustExec(t, db, "PRAGMA recursive_triggers = OFF", "PRAGMA ignore_check_constraints = ON")
	var on int
	if err := db.QueryRow("PRAGMA recursive_triggers").Scan(&on); err != nil || on != 0 {
		t.Fatalf("recursive_triggers = %d (%v), want OFF", on, err)
	}
	return db
}

// zoneKeyStores returns the path of a store the current build opened and the path of the same store as
// an old zone: the build's key guards dropped and nothing else changed, which is what a store that
// predates the guards holds.
func zoneKeyStores(t *testing.T) (current, old string) {
	t.Helper()
	current = zonePreDAGStore(t)
	zoneOpenClose(t, current)
	old = zonePreDAGStore(t)
	zoneOpenClose(t, old)
	db := zoneRawDB(t, old)
	zoneDropKeyGuards(t, db)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return current, old
}

func zoneDropKeyGuards(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, g := range zoneKeyGuards {
		zoneMustExec(t, db, "DROP TRIGGER IF EXISTS "+g.name())
	}
	for _, name := range zoneRowidGuardNames() {
		zoneMustExec(t, db, "DROP TRIGGER IF EXISTS "+name)
	}
	zoneMustExec(t, db, "DROP TABLE IF EXISTS dag_zone_rowid_marks")
}

// zoneTableColumns is table's columns in order with the facts a placeholder row needs.
type zoneColumn struct {
	name    string
	integer bool
	notnull bool
	dflt    bool
}

func zoneTableColumns(t *testing.T, db *sql.DB, table string) []zoneColumn {
	t.Helper()
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []zoneColumn
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			t.Fatal(err)
		}
		out = append(out, zoneColumn{name: name, integer: strings.Contains(strings.ToUpper(typ), "INT"), notnull: notnull == 1, dflt: dflt != nil})
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// keyScenario says how a table's placeholder rows are made. base and other are the values (SQL
// literals) a column has in the stored row and in a row that differs from it; a key column the scenario
// does not name gets 'b-<column>' / 'o-<column>' (1 / 2 for an integer column). fixed columns have the
// base value in both rows. A table's own BEFORE INSERT trigger can need more than that:
//   - settle runs after the base row is stored (the outbox entry leaves the open state, so a second
//     entry of the stream is not refused by the open key as well);
type keyScenario struct {
	base, other map[string]string
	fixed       []string
	settle      []string
}

var zoneKeyScenarios = map[string][]keyScenario{
	"dag_plan_revisions": {{base: map[string]string{"parent_revision_no": "0"}, other: map[string]string{"parent_revision_no": "-1"}}},
	"dag_nodes":          {{base: map[string]string{"retired_rev": "NULL"}, other: map[string]string{"retired_rev": "7"}}},
	// the chain of an agent node: the first scenario's rows have no predecessor (they are in the root key), the second's rows name one (they are in the successor key)
	"dag_agent_executions": {
		{base: map[string]string{"supersedes_execution_id": "NULL"}, fixed: []string{"supersedes_execution_id"}},
		{base: map[string]string{"supersedes_execution_id": "'b-predecessor'"}, fixed: []string{"supersedes_execution_id"}},
	},
	"dag_summary_outbox": {
		// the stream's first entry is open
		{base: map[string]string{"state": "'pending'", "attempts": "0", "plan_revision": "1"}, other: map[string]string{"seq": "2"},
			fixed: []string{"state", "attempts", "plan_revision"}},
		// the same entry once superseded: the stream has no open entry
		{base: map[string]string{"state": "'pending'", "attempts": "0", "plan_revision": "1"}, other: map[string]string{"seq": "2"},
			fixed:  []string{"state", "attempts", "plan_revision"},
			settle: []string{"UPDATE dag_summary_outbox SET state = 'superseded'"}},
	},
}

func (s keyScenario) value(column zoneColumn, other bool) string {
	m := s.base
	if other && !slices.Contains(s.fixed, column.name) {
		m = s.other
	}
	if v, ok := m[column.name]; ok {
		return v
	}
	if other && !slices.Contains(s.fixed, column.name) {
		if column.integer {
			return "2"
		}
		return "'o-" + column.name + "'"
	}
	if column.integer {
		return "1"
	}
	return "'b-" + column.name + "'"
}

// zoneKeyColumns is every column that is part of a key of table or of the WHERE of a partial key.
func zoneKeyColumns(table string) []string {
	var out []string
	for _, g := range zoneKeyGuardsOf(table) {
		out = append(out, g.cols...)
		out = append(out, g.whereCols...)
	}
	return out
}

// zoneKeyRow is an INSERT of one row of table: the columns the placeholder needs (every NOT NULL column
// without a default, and the columns a scenario names), where the columns in keep carry the stored row's
// value and every other key column differs from it. verb is "INSERT", "INSERT OR REPLACE", ...
func zoneKeyRow(verb, table string, columns []zoneColumn, s keyScenario, keep []string) string {
	keyish := zoneKeyColumns(table)
	var names, values []string
	for _, c := range columns {
		_, named := s.base[c.name]
		_, o := s.other[c.name]
		if !c.notnull && !named && !o && !slices.Contains(keyish, c.name) {
			continue
		}
		if c.dflt && !named && !slices.Contains(keyish, c.name) {
			continue
		}
		other := (slices.Contains(keyish, c.name) || o) && !slices.Contains(keep, c.name)
		names = append(names, c.name)
		values = append(values, s.value(c, other))
	}
	return fmt.Sprintf("%s INTO %s (%s) VALUES (%s)", verb, table, strings.Join(names, ","), strings.Join(values, ","))
}

// zoneKeepOf is the columns a row must share with the stored row to conflict on exactly the keys in keys.
func zoneKeepOf(keys []keyGuard) []string {
	var keep []string
	for _, g := range keys {
		keep = append(keep, g.cols...)
		keep = append(keep, g.probe...)
		keep = append(keep, g.whereCols...)
	}
	return keep
}

func zoneScenarioFor(table string, variant int) keyScenario {
	if list, ok := zoneKeyScenarios[table]; ok {
		return list[min(variant, len(list)-1)]
	}
	return keyScenario{}
}

// zoneSeedKeyRow stores the scenario's base row of table and applies its settle statements. The scenario's
func zoneSeedKeyRow(t *testing.T, db *sql.DB, table string, s keyScenario) []zoneColumn {
	t.Helper()
	clearTable(t, db, table)
	columns := zoneTableColumns(t, db, table)
	var all []string
	for _, c := range columns {
		all = append(all, c.name)
	}
	zoneMustExec(t, db, zoneKeyRow("INSERT", table, columns, s, all))
	zoneMustExec(t, db, s.settle...)
	return columns
}

// zoneKeyVariants is how many scenarios a table has.
// skipAgentChainVariant is whether a scenario of dag_agent_executions is outside the key g covers: a row with no predecessor is not in the successor key, a row with one is not in the root key, and the
// primary key is repeated alone by the rows without a predecessor (a row that repeats a predecessor too is refused by the successor key first, which SQLite checks before the primary key).
func skipAgentChainVariant(g keyGuard, variant int) bool {
	return g.table == "dag_agent_executions" && (g.suffix == "root_key" && variant == 1 || g.suffix == "successor_key" && variant == 0 || g.suffix == "pk" && variant == 1)
}

func zoneKeyVariants(table string) int { return max(1, len(zoneKeyScenarios[table])) }

// The guards are in the catalog under their own names with the text the template gives, one per key, and
// together they cover every key of every table that has a <table>_no_delete trigger, derived from the
// catalog: a later append-only table that is not in zoneKeyGuards fails here.
func TestDAGZoneKeyGuardsAreTheTemplateAndCoverEveryAppendOnlyKey(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	catalog := zoneObjects(t, path)
	for _, g := range zoneKeyGuards {
		held, ok := catalog["trigger "+g.name()]
		if !ok {
			t.Errorf("trigger %s is not in the zone", g.name())
			continue
		}
		want := normalizeSQL(strings.Replace(zoneKeyGuardSQL(g), "IF NOT EXISTS ", "", 1))
		if held != want {
			t.Errorf("trigger %s\n got  %s\n want %s", g.name(), held, want)
		}
	}

	for _, table := range zoneKeyGuardTables() {
		before, mark, after := zoneRowidGuardSQL(table, zoneRowidEarlier(table))
		for name, text := range map[string]string{table + "_rowid_guard": before, table + "_rowid_minus1_mark_guard": mark, table + "_rowid_minus1_guard": after} {
			held, ok := catalog["trigger "+name]
			want := normalizeSQL(strings.Replace(text, "IF NOT EXISTS ", "", 1))
			if !ok || held != want {
				t.Errorf("trigger %s\n got  %s\n want %s", name, held, want)
			}
		}
	}
	if held, want := catalog["table dag_zone_rowid_marks"], normalizeSQL(strings.Replace(zoneRowidMarksSQL, "IF NOT EXISTS ", "", 1)); held != want {
		t.Errorf("table dag_zone_rowid_marks\n got  %s\n want %s", held, want)
	}

	db := zoneRawDB(t, path)
	rows, err := db.Query("SELECT tbl_name FROM sqlite_master WHERE type = 'trigger' AND name = tbl_name || '_no_delete' ORDER BY tbl_name")
	if err != nil {
		t.Fatal(err)
	}
	var appendOnly []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		appendOnly = append(appendOnly, name)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	tables := zoneKeyGuardTables()
	sort.Strings(tables)
	if !slices.Equal(appendOnly, tables) {
		t.Fatalf("append-only tables (a _no_delete trigger) = %v, guarded tables = %v", appendOnly, tables)
	}
	for _, table := range appendOnly {
		// the rowid guards stand for the table's implicit key: the table is a rowid table and none of its columns is the rowid
		var withoutRowid int
		if err := db.QueryRow("SELECT wr FROM pragma_table_list WHERE name = ?", table).Scan(&withoutRowid); err != nil || withoutRowid != 0 {
			t.Errorf("%s: WITHOUT ROWID = %d (%v); the rowid guards are for rowid tables", table, withoutRowid, err)
		}
		if got := zoneStringList(t, db, "SELECT name FROM pragma_table_info('"+table+"') WHERE pk = 1 AND upper(type) = 'INTEGER' AND (SELECT count(*) FROM pragma_table_info('"+table+"') WHERE pk > 0) = 1"); len(got) != 0 {
			t.Errorf("%s: %v is an INTEGER PRIMARY KEY, an alias of the rowid: its message is the key's, not the rowid guard's", table, got)
		}
		var have []string
		idx, err := db.Query("SELECT name, partial FROM pragma_index_list('" + table + "') WHERE \"unique\" = 1")
		if err != nil {
			t.Fatal(err)
		}
		type index struct {
			name    string
			partial bool
		}
		var indexes []index
		for idx.Next() {
			var i index
			var partial int
			if err := idx.Scan(&i.name, &partial); err != nil {
				t.Fatal(err)
			}
			i.partial = partial == 1
			indexes = append(indexes, i)
		}
		if err := errors.Join(idx.Err(), idx.Close()); err != nil {
			t.Fatal(err)
		}
		for _, i := range indexes {
			cols := zoneStringList(t, db, "SELECT name FROM pragma_index_info('"+i.name+"') ORDER BY seqno")
			have = append(have, strings.Join(cols, ","))
			var found bool
			for _, g := range zoneKeyGuardsOf(table) {
				if strings.Join(g.cols, ",") != strings.Join(cols, ",") {
					continue
				}
				found = true
				if (g.where != "") != i.partial {
					t.Errorf("%s key (%s): partial = %v, the guard's where = %q", table, strings.Join(cols, ","), i.partial, g.where)
				}
				if i.partial {
					var ddl string
					if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE name = ?", i.name).Scan(&ddl); err != nil {
						t.Fatal(err)
					}
					if !strings.HasSuffix(normalizeSQL(ddl), "WHERE "+normalizeSQL(g.where)) {
						t.Errorf("%s: the index %q does not end with the guard's WHERE %q", table, ddl, g.where)
					}
				}
			}
			if _, exempt := zoneKeyGuardExempt[table][strings.Join(cols, ",")]; !found && !exempt {
				t.Errorf("%s: no key guard for the unique key (%s)", table, strings.Join(cols, ","))
			}
		}
		if len(zoneKeyGuardsOf(table))+len(zoneKeyGuardExempt[table]) != len(indexes) {
			t.Errorf("%s: %d guards for %d unique keys %v", table, len(zoneKeyGuardsOf(table)), len(indexes), have)
		}
	}
}

func zoneStringList(t *testing.T, db *sql.DB, query string) []string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatal(err)
		}
		out = append(out, s)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		t.Fatal(err)
	}
	return out
}

// For every key of every append-only table, an INSERT OR REPLACE that repeats only that key succeeds on an
// old zone (the red case: the stored row is gone and the new one stands in its place, with no trigger
// run) and is refused on the current one with the key's own text, leaving the stored row as it was.
func TestDAGZoneRefusesAnInsertOrReplaceOfAStoredKey(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for _, g := range zoneKeyGuards {
		for variant := 0; variant < zoneKeyVariants(g.table); variant++ {
			scenario := zoneScenarioFor(g.table, variant)
			if g.table == "dag_summary_outbox" && g.suffix == "open_key" && variant == 1 {
				continue // the settled entry is not open: its key is free
			}
			if g.table == "dag_summary_outbox" && g.suffix != "open_key" && variant == 0 {
				continue // the open entry conflicts on the open key too, which is checked first
			}
			if skipAgentChainVariant(g, variant) {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", g.table, g.suffix), func(t *testing.T) {
				for _, which := range []struct {
					name string
					path string
					want bool
				}{{"old zone", old, false}, {"current", current, true}} {
					db := zoneKeyDB(t, which.path)
					columns := zoneSeedKeyRow(t, db, g.table, scenario)
					before := zoneTableContents(t, db, g.table)
					statement := zoneKeyRow("INSERT OR REPLACE", g.table, columns, scenario, zoneKeepOf([]keyGuard{g}))
					tx, err := db.Begin()
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(statement)
					inTx := zoneTableContents(t, tx, g.table)
					_ = tx.Rollback()
					if which.want {
						if err == nil || !strings.Contains(err.Error(), g.message()) {
							t.Fatalf("%s: %s: %v; want the refusal %q", which.name, statement, err, g.message())
						}
					} else if err != nil {
						t.Fatalf("%s: %s: %v; the red case is a REPLACE that succeeds", which.name, statement, err)
					}
					zoneCheckStatementEffect(t, which.name+": "+statement, before, inTx, err == nil)
					if after := zoneTableContents(t, db, g.table); !slices.Equal(before, after) {
						t.Fatalf("%s: the table changed over a refused or rolled-back REPLACE:\n%q\n%q", which.name, before, after)
					}
				}
			})
		}
	}
}

// zoneCheckStatementEffect compares the table as a refused statement left it, read inside its transaction
// before the rollback, with the table as it was: the statement itself must have changed no row, which a
// comparison made after the caller's rollback cannot show. An accepted statement (the old zone's REPLACE)
// may change the table, or leave it equal when it repeats the whole row, so nothing is required of it.
func zoneCheckStatementEffect(t *testing.T, what string, before, inTx []string, accepted bool) {
	t.Helper()
	if !accepted && !slices.Equal(before, inTx) {
		t.Fatalf("%s: the refused statement changed the table before the rollback:\n%q\n%q", what, before, inTx)
	}
}

// clearTable empties table: the no-delete trigger is dropped, the rows are deleted and the trigger is created again from its stored text.
func clearTable(t *testing.T, db *sql.DB, table string) {
	t.Helper()
	var name string
	if err := db.QueryRow("SELECT name FROM sqlite_master WHERE type = 'trigger' AND name = ?", table+"_no_delete").Scan(&name); err != nil {
		t.Fatal(err)
	}
	var ddl string
	if err := db.QueryRow("SELECT sql FROM sqlite_master WHERE type = 'trigger' AND name = ?", name).Scan(&ddl); err != nil {
		t.Fatal(err)
	}
	zoneMustExec(t, db, "DROP TRIGGER "+name, "DELETE FROM "+table, ddl)
}

// A plain INSERT that repeats a key keeps its refusal text and its class: IntegrityError (primary code 19)
// with the text SQLite gives today. The extended code of the refusal is the guard's (trigger, 1811).
func TestDAGZoneKeyGuardsKeepThePlainInsertRefusal(t *testing.T) {
	t.Parallel()
	current, _ := zoneKeyStores(t)
	for _, g := range zoneKeyGuards {
		for variant := 0; variant < zoneKeyVariants(g.table); variant++ {
			scenario := zoneScenarioFor(g.table, variant)
			if g.table == "dag_summary_outbox" && (g.suffix == "open_key") == (variant == 1) {
				continue
			}
			if skipAgentChainVariant(g, variant) {
				continue
			}
			t.Run(fmt.Sprintf("%s/%s", g.table, g.suffix), func(t *testing.T) {
				db := zoneKeyDB(t, current)
				columns := zoneSeedKeyRow(t, db, g.table, scenario)
				_, err := db.Exec(zoneKeyRow("INSERT", g.table, columns, scenario, zoneKeepOf([]keyGuard{g})))
				if err == nil {
					t.Fatalf("a repeated key was accepted")
				}
				if got := sqliteMessage(err); got != g.message() {
					t.Fatalf("refusal text = %q, want %q", got, g.message())
				}
				var failure *sqlite.Error
				if !errors.As(err, &failure) || failure.Code()&0xff != 19 {
					t.Fatalf("refusal = %v, want a constraint error (primary code 19)", err)
				}
				if failure.Code() != 1811 {
					t.Fatalf("extended code = %d, want 1811 (SQLITE_CONSTRAINT_TRIGGER)", failure.Code())
				}
				if class := StoredSQLiteError(err); class != "IntegrityError: "+g.message() {
					t.Fatalf("stored class = %q", class)
				}
				if RefusalReason(err) != "" {
					t.Fatalf("a key refusal became a store refusal: %v", err)
				}
			})
		}
	}
}

// When an insert repeats several keys at once the refusal text, which is the priority among the keys, is
// the one the old zone gave: every subset of a table's keys is repeated on the old zone and on the current
// store and the two answers (the text, or success) are the same.
func TestDAGZoneKeyGuardsKeepThePriorityAmongKeys(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for _, table := range zoneKeyGuardTables() {
		keys := zoneKeyGuardsOf(table)
		for variant := 0; variant < zoneKeyVariants(table); variant++ {
			scenario := zoneScenarioFor(table, variant)
			t.Run(fmt.Sprintf("%s/%d", table, variant), func(t *testing.T) {
				answers := map[string][]string{}
				for name, path := range map[string]string{"old": old, "current": current} {
					db := zoneKeyDB(t, path)
					columns := zoneSeedKeyRow(t, db, table, scenario)
					for mask := 1; mask < 1<<len(keys); mask++ {
						var subset []keyGuard
						for i, g := range keys {
							if mask&(1<<i) != 0 {
								subset = append(subset, g)
							}
						}
						tx, err := db.Begin()
						if err != nil {
							t.Fatal(err)
						}
						_, err = tx.Exec(zoneKeyRow("INSERT", table, columns, scenario, zoneKeepOf(subset)))
						_ = tx.Rollback()
						answer := "accepted"
						if err != nil {
							answer = sqliteMessage(err)
						}
						answers[name] = append(answers[name], answer)
					}
				}
				if !slices.Equal(answers["old"], answers["current"]) {
					t.Fatalf("the answers to repeated key subsets differ:\n old     %q\n current %q", answers["old"], answers["current"])
				}
			})
		}
	}
}

// A guard stands aside for the table's own earlier BEFORE INSERT trigger: a row both of them refuse keeps
// the earlier trigger's message, as on the old zone.
func TestDAGZoneKeyGuardsLeaveTheEarlierTriggerItsMessage(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for name, path := range map[string]string{"old": old, "current": current} {
		db := zoneKeyDB(t, path)
		// outbox: a repeated summary_id with a sequence number that is not the next one
		zoneMustExec(t, db, zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1))
		err := zoneExec(t, db, zoneSummaryOutboxRow("s-1", "plan-1", "doc", 5))
		if err == nil || !strings.Contains(err.Error(), "an entry is appended pending, with the next sequence number") {
			t.Errorf("%s: outbox: %v; want the order trigger's message", name, err)
		}
		// revisions: a repeated request with a parent revision that does not exist
		zoneMustExec(t, db, "INSERT INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, author_task_id, recorded_at) VALUES ('p',1,0,'r1','d','{}','s','t','x')")
		err = zoneExec(t, db, "INSERT INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, author_task_id, recorded_at) VALUES ('p',9,8,'r1','d','{}','s','t','x')")
		if err == nil || !strings.Contains(err.Error(), "the parent revision does not exist") {
			t.Errorf("%s: revisions: %v; want the parent trigger's message", name, err)
		}
	}
}

func zoneSummaryOutboxRow(id, plan, document string, seq int) string {
	return fmt.Sprintf("INSERT INTO dag_summary_outbox (summary_id, plan_id, project_key, document, plan_revision, seq, subject_digest, state_digest, summary, summary_sha256, state, enqueued_by, created_at, updated_at)"+
		" VALUES ('%s','%s','PRJ','%s',1,%d,'%s','sd','text','sha','pending','task-a','2026-10-06T00:00:00Z','2026-10-06T00:00:00Z')",
		id, plan, document, seq, zoneDigest)
}

// The one conflict the old zone refused before its CHECKs is a conflict the guard now names first: a row that
// repeats a key and also breaks a CHECK of its table was refused by the CHECK, and is refused by the key
// guard's text now, because a BEFORE trigger runs before SQLite checks the row's constraints. Either way
// the row is refused and nothing is written.
func TestDAGZoneKeyGuardNamesTheKeyBeforeACheckOfTheSameRow(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for name, path := range map[string]string{"old": old, "current": current} {
		db := zoneRawDB(t, path) // CHECK constraints on
		zoneMustExec(t, db, "INSERT INTO merge_trains VALUES ('t-1','k','o/r','dev','sha','lead','2026-10-06T00:00:00Z')")
		err := zoneExec(t, db, "INSERT INTO merge_trains VALUES ('t-1','','o/r','dev','sha','lead','2026-10-06T00:00:00Z')")
		want := "CHECK constraint failed"
		if name == "current" {
			want = "UNIQUE constraint failed: merge_trains.train_id"
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v; want %q", name, err, want)
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM merge_trains").Scan(&n); err != nil || n != 1 {
			t.Errorf("%s: %d rows (%v), want 1", name, n, err)
		}
	}
}

// Rows that do not repeat a key are written, and the paths the tables allow are untouched by the guards: a
// node and an edge retire (an UPDATE), a retired key is free for a live version of the same node, an
// outbox entry moves through its states and leaves its stream's open key free for the next entry, and a
// partial key admits any number of rows that are not in its index.
func TestDAGZoneKeyGuardsLeaveTheNormalPathsAlone(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneKeyDB(t, path)
	zoneMustExec(t, db,
		"INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-06T00:00:00Z')",
		"INSERT INTO dag_plans VALUES ('plan-2','PRJ-A','task-a','2026-10-06T00:00:00Z')",
	)
	node := func(plan string, rev int, retired string) string {
		return fmt.Sprintf("INSERT INTO dag_nodes (plan_id,node_id,introduced_rev,retired_rev,slice_digest,issue_key,node_kind,criteria_set_digest) VALUES ('%s','n',%d,%s,'s','K-1','implementation','c')", plan, rev, retired)
	}
	zoneMustExec(t, db,
		node("plan-1", 1, "NULL"),
		node("plan-2", 1, "NULL"), // same node id, another plan
		"UPDATE dag_nodes SET retired_rev = 2 WHERE plan_id = 'plan-1' AND node_id = 'n' AND introduced_rev = 1",
		node("plan-1", 2, "NULL"), // the live key is free after the retirement
		node("plan-1", 3, "5"),    // retired rows are not in the live index: several of the same node
		node("plan-1", 4, "5"),
	)
	if err := zoneExec(t, db, node("plan-1", 5, "NULL")); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_nodes.plan_id, dag_nodes.node_id") {
		t.Fatalf("a second live version: %v", err)
	}
	zoneMustExec(t, db,
		zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1),
		zoneSummaryOutboxRow("s-x", "plan-1", "other", 1), // another stream
		"UPDATE dag_summary_outbox SET state = 'claimed', claim_token = 'c', claimed_by = 'w', claimed_at = 'x' WHERE summary_id = 's-1'",
		"UPDATE dag_summary_outbox SET state = 'superseded', claim_token = 'c' WHERE summary_id = 's-1'",
		zoneSummaryOutboxRow("s-2", "plan-1", "doc", 2), // the stream's open key is free again
	)
	if err := zoneExec(t, db, zoneSummaryOutboxRow("s-3", "plan-1", "doc", 3)); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_summary_outbox.plan_id, dag_summary_outbox.document") {
		t.Fatalf("a second open entry: %v", err)
	}
}

// UNIQUE keeps its NULL meaning: rows whose key holds a NULL are distinct, so the guards (which compare with =)
// admit what the keys admit. A TEXT PRIMARY KEY of a rowid table holds NULL, here on a store whose NULL-key
// guards (CRW-835) are dropped to write the rows the way an old zone holds them.
func TestDAGZoneKeyGuardsKeepTheNullMeaningOfUnique(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for name, path := range map[string]string{"old": old, "current": current} {
		db := zoneKeyDB(t, path)
		zoneMustExec(t, db, "DROP TRIGGER dag_plans_plan_id_not_null")
		for i := 0; i < 2; i++ {
			zoneMustExec(t, db, "INSERT INTO dag_plans VALUES (NULL,'PRJ-A','task-a','2026-10-06T00:00:00Z')")
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM dag_plans WHERE plan_id IS NULL").Scan(&n); err != nil || n != 2 {
			t.Errorf("%s: %d NULL-key rows (%v), want 2", name, n, err)
		}
		// a NULL key does not make a REPLACE of another row refuse, and the NULL rows stay
		zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('p-1','PRJ-A','task-a','2026-10-06T00:00:00Z')")
		if err := zoneExec(t, db, "INSERT OR REPLACE INTO dag_plans VALUES (NULL,'PRJ-B','task-a','2026-10-06T00:00:00Z')"); err != nil {
			t.Errorf("%s: a NULL key REPLACE: %v", name, err)
		}
	}
}

// A store that holds the old zone (rows in it, the guards absent) opens, gains the guards, keeps every row,
// and refuses a REPLACE of a stored key from then on.
func TestDAGZoneUpgradeAddsTheKeyGuardsAndKeepsTheRows(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	old := zoneKeyDB(t, path)
	zoneDropKeyGuards(t, old)
	contents := map[string][]string{}
	for _, table := range []string{"dag_plans", "merge_trains", "dag_verified_heads"} {
		scenario := zoneScenarioFor(table, 0)
		zoneSeedKeyRow(t, old, table, scenario)
		contents[table] = zoneTableContents(t, old, table)
	}
	// an old zone's REPLACE worked: the row is rewritten, which is the defect
	zoneMustExec(t, old, "INSERT OR REPLACE INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES ('b-event_id','r',1,'t','rewritten','x','y')")
	contents["dag_verified_heads"] = zoneTableContents(t, old, "dag_verified_heads")
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}
	if missing := zoneMissingGuards(t, path); len(missing) != len(zoneKeyGuards)+len(zoneRowidGuardNames()) {
		t.Fatalf("the old zone lacks %d guards, want all %d", len(missing), len(zoneKeyGuards)+len(zoneRowidGuardNames()))
	}

	zoneOpenClose(t, path)
	if missing := zoneMissingGuards(t, path); len(missing) != 0 {
		t.Fatalf("guards still missing after the open: %v", missing)
	}
	db := zoneKeyDB(t, path)
	for table, want := range contents {
		if got := zoneTableContents(t, db, table); !slices.Equal(got, want) {
			t.Errorf("%s: rows changed over the upgrade:\n%q\n%q", table, got, want)
		}
	}
	err := zoneExec(t, db, "INSERT OR REPLACE INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES ('b-event_id','r',1,'t','again','x','y')")
	if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_verified_heads.event_id") {
		t.Fatalf("a REPLACE after the upgrade: %v", err)
	}
	if got := zoneTableContents(t, db, "dag_verified_heads"); !slices.Equal(got, contents["dag_verified_heads"]) {
		t.Fatalf("the stored row changed over the refused REPLACE: %q", got)
	}
}

// zoneMissingGuards is the read-only post-install check (MissingZoneKeyGuards) on the file at path.
func zoneMissingGuards(t *testing.T, path string) []string {
	t.Helper()
	db := zoneRawDB(t, path)
	missing, err := MissingZoneKeyGuards(context.Background(), db)
	if err != nil {
		t.Fatal(err)
	}
	return missing
}

// MissingZoneKeyGuards reads the catalog and changes nothing: it names the guards a store lacks, none on a
// store the build opened, and it does not create one on a store that lacks them.
func TestMissingZoneKeyGuardsIsReadOnly(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	if missing := zoneMissingGuards(t, current); len(missing) != 0 {
		t.Fatalf("a store the build opened lacks %v", missing)
	}
	before, err := os.ReadFile(old)
	if err != nil {
		t.Fatal(err)
	}
	missing := zoneMissingGuards(t, old)
	var want []string
	for _, g := range zoneKeyGuards {
		want = append(want, g.name())
	}
	want = append(want, zoneRowidGuardNames()...)
	sort.Strings(want)
	if !slices.Equal(missing, want) {
		t.Fatalf("missing = %v, want every guard %v", missing, want)
	}
	if after, err := os.ReadFile(old); err != nil || string(before) != string(after) {
		t.Fatalf("the check changed the file (%v)", err)
	}
	// the check also reads a store that is opened read-only
	ro, err := sql.Open("sqlite", "file:"+filepath.ToSlash(old)+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if got, err := MissingZoneKeyGuards(context.Background(), ro); err != nil || len(got) != len(want) {
		t.Fatalf("read-only open: %v (%v)", got, err)
	}
}

// Statements that skip a conflicting row are for tables a writer means them for. This build's product
// code, read from its sources, writes none of the guarded tables with INSERT OR REPLACE, REPLACE INTO,
// INSERT OR IGNORE or ON CONFLICT, so the guard (which refuses those statements on a stored key) rejects
// no writer. The idempotent writers (dag_node_executions, dag_input_manifests, dag_conflict_drift) write
// tables that are not guarded and stay no-ops (TestDAGZoneIdempotentWritersStayNoOps).
func TestNoProductWriterRelaxesAGuardedTable(t *testing.T) {
	t.Parallel()
	var found []string
	root := filepath.Join("..", "..", "..")
	err := filepath.WalkDir(filepath.Join(root, "internal"), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, statement := range zoneRelaxingWriters(string(data), zoneKeyGuardTables()) {
			found = append(found, filepath.ToSlash(path)+": "+statement)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 0 {
		t.Fatalf("a product writer relaxes a guarded table; list it as an exception or change it:\n%s", strings.Join(found, "\n"))
	}
}

// zoneRelaxingWriters finds, in Go source text, the statements that write one of tables with INSERT OR REPLACE,
// REPLACE INTO, INSERT OR IGNORE (or another OR clause) or ON CONFLICT. A statement built from adjacent string
// literals reads as one run once the quotes between them go.
func zoneRelaxingWriters(source string, tables []string) []string {
	into := regexp.MustCompile(`(?is)(INSERT\s+(?:OR\s+\w+\s+)?|REPLACE\s+)INTO\s+(` + strings.Join(tables, "|") + `)\b([^;` + "`" + `]{0,900})`)
	conflict := regexp.MustCompile(`(?i)ON\s+CONFLICT`)
	text := strings.NewReplacer(`"+`+"\n", "", `"`, " ", "\t", " ").Replace(source)
	var out []string
	for _, m := range into.FindAllStringSubmatch(text, -1) {
		verb := strings.ToUpper(strings.Join(strings.Fields(m[1]), " "))
		if verb != "INSERT" || conflict.MatchString(strings.SplitN(m[3], "INSERT", 2)[0]) {
			out = append(out, strings.Join(strings.Fields(m[0]), " "))
		}
	}
	return out
}

// The scanner finds what it is for: each relaxed form on a guarded table, split over adjacent literals too, and
// none of the plain inserts or of the idempotent writers of tables that are not guarded.
func TestZoneRelaxingWritersScannerFindsTheRelaxedForms(t *testing.T) {
	t.Parallel()
	tables := zoneKeyGuardTables()
	for _, source := range []string{
		`x.Exec("INSERT OR REPLACE INTO dag_verified_heads (a) VALUES (?)")`,
		`x.Exec("REPLACE INTO merge_trains VALUES (?)")`,
		`x.Exec("INSERT OR IGNORE INTO dag_plans (plan_id) VALUES (?)")`,
		"x.Exec(\"INSERT INTO dag_nodes (a)\"+\n\t\" VALUES (?)\"+\n\t\" ON CONFLICT (plan_id) DO NOTHING\")",
	} {
		if got := zoneRelaxingWriters(source, tables); len(got) != 1 {
			t.Errorf("%q: found %v, want one statement", source, got)
		}
	}
	for _, source := range []string{
		`x.Exec("INSERT INTO dag_verified_heads (a) VALUES (?)")`,
		`x.Exec("INSERT INTO dag_plans (a) VALUES (?)"); x.Exec("INSERT INTO other (a) VALUES (?) ON CONFLICT DO NOTHING")`,
		`x.Exec("INSERT OR IGNORE INTO dag_input_manifests (a) VALUES (?)")`,
		`x.Exec("INSERT INTO dag_node_executions (a) VALUES (?) ON CONFLICT (b) DO NOTHING")`,
		`x.Exec("INSERT OR IGNORE INTO dag_conflict_drift (a) VALUES (?)")`,
	} {
		if got := zoneRelaxingWriters(source, tables); len(got) != 0 {
			t.Errorf("%q: found %v, want none", source, got)
		}
	}
}

// The writers that are idempotent by design write tables that are not guarded, and still skip a repeat
// without an error: INSERT OR IGNORE and ON CONFLICT DO NOTHING (release.go, manifest.go, livesweep.go).
func TestDAGZoneIdempotentWritersStayNoOps(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneKeyDB(t, path)
	for _, table := range []string{"dag_node_executions", "dag_input_manifests", "dag_conflict_drift"} {
		if slices.Contains(zoneKeyGuardTables(), table) {
			t.Fatalf("%s is idempotent by design and must not be guarded", table)
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM sqlite_master WHERE type = 'trigger' AND tbl_name = ? AND name LIKE '%_guard'", table).Scan(&n); err != nil || n != 0 {
			t.Fatalf("%s has %d guards (%v)", table, n, err)
		}
	}
	zoneMustExec(t, db, "PRAGMA ignore_check_constraints = ON")
	insertExecution := "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('p','n','rel',1,'m','initial','req')" +
		" ON CONFLICT (relationship_id, execution_generation) DO NOTHING"
	manifest := "INSERT OR IGNORE INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES ('d','n','{}','{}',0,'x')"
	drift := "INSERT OR IGNORE INTO dag_conflict_drift (observation_id, node_id, path) VALUES ('o','n','p')"
	for _, statement := range []string{insertExecution, manifest, drift} {
		zoneMustExec(t, db, statement, statement)
	}
	for table, want := range map[string]int{"dag_node_executions": 1, "dag_input_manifests": 1, "dag_conflict_drift": 1} {
		var n int
		if err := db.QueryRow("SELECT count(*) FROM " + table).Scan(&n); err != nil || n != want {
			t.Errorf("%s holds %d rows (%v), want %d", table, n, err, want)
		}
	}
}

// What a guard does to the statements the product does not write, pinned so a later change is seen: an
// INSERT OR IGNORE or an ON CONFLICT DO NOTHING that repeats a stored key of a guarded table is refused
// (RAISE(ABORT) is not softened by the statement's conflict clause) and writes nothing.
func TestDAGZoneKeyGuardRefusesIgnoreAndDoNothingOnAStoredKey(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneKeyDB(t, path)
	zoneMustExec(t, db, "INSERT INTO merge_trains VALUES ('t-1','k','o/r','dev','sha','lead','2026-10-06T00:00:00Z')")
	for _, statement := range []string{
		"INSERT OR IGNORE INTO merge_trains VALUES ('t-1','k2','o/r','dev','sha','lead','x')",
		"INSERT INTO merge_trains VALUES ('t-1','k2','o/r','dev','sha','lead','x') ON CONFLICT DO NOTHING",
	} {
		if err := zoneExec(t, db, statement); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: merge_trains.train_id") {
			t.Errorf("%s: %v", statement, err)
		}
	}
	var n int
	if err := db.QueryRow("SELECT count(*) FROM merge_trains").Scan(&n); err != nil || n != 1 {
		t.Fatalf("%d rows (%v)", n, err)
	}
}

// The outbox red case of the issue: an entry that keeps its summary_id and takes the next sequence number of
// its stream replaces the stored entry (which was superseded) on an old zone, and is refused on the current
// one with the order trigger intact. The same entry as a plain INSERT is refused on the summary_id.
func TestDAGZoneRefusesAnOutboxReplaceThatKeepsTheSummaryIDAndTakesTheNextSequence(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for name, path := range map[string]string{"old": old, "current": current} {
		db := zoneKeyDB(t, path)
		zoneMustExec(t, db,
			zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1),
			"UPDATE dag_summary_outbox SET state = 'superseded' WHERE summary_id = 's-1'",
		)
		replace := strings.Replace(zoneSummaryOutboxRow("s-1", "plan-1", "doc", 2), "INSERT INTO", "INSERT OR REPLACE INTO", 1)
		err := zoneExec(t, db, replace)
		if name == "old" {
			if err != nil {
				t.Fatalf("old zone: %v; the red case is a REPLACE that succeeds", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_summary_outbox.summary_id") {
			t.Fatalf("current: %v; want the summary_id refusal", err)
		}
		plain := zoneSummaryOutboxRow("s-1", "plan-1", "doc", 2)
		if err := zoneExec(t, db, plain); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: dag_summary_outbox.summary_id") {
			t.Fatalf("plain INSERT: %v", err)
		}
		var n int
		if err := db.QueryRow("SELECT count(*) FROM dag_summary_outbox").Scan(&n); err != nil || n != 1 {
			t.Fatalf("%d entries (%v), want the stored one", n, err)
		}
	}
}

// The exempt key of the outbox is refused today by the order trigger, for a plain INSERT and a REPLACE alike,
// with that trigger's message; no guard is added for it (zoneKeyGuardExempt).
func TestDAGZoneOutboxSequenceKeyIsRefusedByTheOrderTrigger(t *testing.T) {
	t.Parallel()
	_, old := zoneKeyStores(t)
	current, _ := zoneKeyStores(t)
	for name, path := range map[string]string{"old": old, "current": current} {
		db := zoneKeyDB(t, path)
		zoneMustExec(t, db, zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1), "UPDATE dag_summary_outbox SET state = 'superseded'")
		for _, verb := range []string{"INSERT", "INSERT OR REPLACE"} {
			statement := strings.Replace(zoneSummaryOutboxRow("s-2", "plan-1", "doc", 1), "INSERT", verb, 1)
			if err := zoneExec(t, db, statement); err == nil || !strings.Contains(err.Error(), "an entry is appended pending, with the next sequence number") {
				t.Errorf("%s: %s: %v; want the order trigger's refusal", name, verb, err)
			}
		}
	}
}

// An INSERT OR REPLACE that writes a NULL into attempts is the bypass of the outbox guards' stand-aside condition:
// the order trigger's WHEN is NULL there (NEW.attempts <> 0), so it does not fire, and a guard that stands aside under
// NOT (that WHEN) would stand aside too; REPLACE then turns the NULL into the column's default and replaces the
// stored entry. Both outbox keys are refused whatever the NULL does to the order trigger's condition.
func TestDAGZoneKeyGuardsRefuseAnOutboxReplaceWithANullAttempts(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	withNullAttempts := func(statement string) string {
		statement = strings.Replace(statement, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
		statement = strings.Replace(statement, "(summary_id,", "(attempts, summary_id,", 1)
		return strings.Replace(statement, "VALUES (", "VALUES (NULL,", 1)
	}
	cases := []struct {
		name    string
		seed    []string
		replace string
		want    string
	}{
		{"the stored summary_id with the next sequence number of a superseded stream", []string{zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1), "UPDATE dag_summary_outbox SET state = 'superseded'"},
			zoneSummaryOutboxRow("s-1", "plan-1", "doc", 2), ""},
		{"another summary_id in the stream whose entry is open", []string{zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1)},
			zoneSummaryOutboxRow("s-2", "plan-1", "doc", 2), "UNIQUE constraint failed: dag_summary_outbox.plan_id, dag_summary_outbox.document"},
		{"the stored summary_id with the next sequence number of an open stream", []string{zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1)},
			zoneSummaryOutboxRow("s-1", "plan-1", "doc", 2), "UNIQUE constraint failed: dag_summary_outbox."},
		{"a sequence number that is not the next one keeps the order trigger's message", []string{zoneSummaryOutboxRow("s-1", "plan-1", "doc", 1)},
			zoneSummaryOutboxRow("s-1", "plan-1", "doc", 5), "an entry is appended pending, with the next sequence number"},
	}
	cases[0].want = "UNIQUE constraint failed: dag_summary_outbox.summary_id"
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, which := range []struct {
				name string
				path string
			}{{"old zone", old}, {"current", current}} {
				db := zoneKeyDB(t, which.path)
				clearTable(t, db, "dag_summary_outbox")
				zoneMustExec(t, db, c.seed...)
				before := zoneTableContents(t, db, "dag_summary_outbox")
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(withNullAttempts(c.replace))
				inTx := zoneTableContents(t, tx, "dag_summary_outbox")
				_ = tx.Rollback()
				orderRefusal := strings.Contains(c.want, "next sequence number")
				switch {
				case which.name == "old zone" && !orderRefusal:
					if err != nil {
						t.Fatalf("old zone: %v; the red case is a REPLACE that succeeds", err)
					}
				default:
					if err == nil || !strings.Contains(err.Error(), c.want) {
						t.Fatalf("%s: %v; want %q", which.name, err, c.want)
					}
				}
				zoneCheckStatementEffect(t, which.name+": "+c.name, before, inTx, err == nil)
				if after := zoneTableContents(t, db, "dag_summary_outbox"); !slices.Equal(before, after) {
					t.Fatalf("%s: the table changed:\n%q\n%q", which.name, before, after)
				}
			}
		})
	}
}

// zoneRowidStatement is the INSERT of a row that differs from the seeded one in every key and names a rowid.
func zoneRowidStatement(verb, table, rowidName, rowid string, columns []zoneColumn, s keyScenario) string {
	statement := zoneKeyRow(verb, table, columns, s, zoneRowidKeep(table))
	statement = strings.Replace(statement, "INTO "+table+" (", "INTO "+table+" ("+rowidName+",", 1)
	return strings.Replace(statement, "VALUES (", "VALUES ("+rowid+",", 1)
}

// zoneRowidKeep are the columns the new row shares with the seeded one so the table's earlier trigger lets it
// through: the outbox entry joins the (settled) stream of the seeded one.
func zoneRowidKeep(table string) []string {
	if table == "dag_summary_outbox" {
		return []string{"plan_id", "document"}
	}
	return nil
}

func zoneRowidScenario(table string) keyScenario {
	if table == "dag_summary_outbox" {
		return zoneScenarioFor(table, 1) // the stream has no open entry: the new entry is the next of it
	}
	return zoneScenarioFor(table, 0)
}

// An INSERT OR REPLACE that names the rowid of a stored row, by any of its three names, and shares no declared
// key with it replaces the row on an old zone and is refused on the current one with the text a plain INSERT of
// the rowid gets; a row that names a free rowid, or none, is written.
func TestDAGZoneRefusesAnInsertOrReplaceOfAStoredRowid(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	for _, table := range zoneKeyGuardTables() {
		t.Run(table, func(t *testing.T) {
			scenario := zoneRowidScenario(table)
			for _, which := range []struct {
				name string
				path string
				want bool
			}{{"old zone", old, false}, {"current", current, true}} {
				db := zoneKeyDB(t, which.path)
				columns := zoneSeedKeyRow(t, db, table, scenario)
				var stored int64
				if err := db.QueryRow("SELECT rowid FROM " + table).Scan(&stored); err != nil {
					t.Fatal(err)
				}
				before := zoneTableContents(t, db, table)
				for _, name := range []string{"rowid", "_rowid_", "oid"} {
					for _, verb := range []string{"INSERT OR REPLACE", "INSERT"} {
						statement := zoneRowidStatement(verb, table, name, fmt.Sprint(stored), columns, scenario)
						tx, err := db.Begin()
						if err != nil {
							t.Fatal(err)
						}
						_, err = tx.Exec(statement)
						inTx := zoneTableContents(t, tx, table)
						_ = tx.Rollback()
						zoneCheckStatementEffect(t, which.name+": "+statement, before, inTx, err == nil)
						switch {
						case verb == "INSERT OR REPLACE" && !which.want:
							if err != nil {
								t.Fatalf("%s: %s: %v; the red case is a REPLACE that succeeds", which.name, statement, err)
							}
						default:
							if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".rowid") {
								t.Fatalf("%s: %s: %v; want the refusal of the rowid", which.name, statement, err)
							}
						}
						if after := zoneTableContents(t, db, table); !slices.Equal(before, after) {
							t.Fatalf("%s: %s: the table changed:\n%q\n%q", which.name, statement, before, after)
						}
					}
				}
				// a free rowid and an assigned one are written
				for _, rowid := range []string{fmt.Sprint(stored + 100), "-2"} {
					tx, err := db.Begin()
					if err != nil {
						t.Fatal(err)
					}
					_, err = tx.Exec(zoneRowidStatement("INSERT OR REPLACE", table, "rowid", rowid, columns, scenario))
					_ = tx.Rollback()
					if err != nil {
						t.Fatalf("%s: rowid %s is free: %v", which.name, rowid, err)
					}
				}
				tx, err := db.Begin()
				if err != nil {
					t.Fatal(err)
				}
				_, err = tx.Exec(zoneKeyRow("INSERT", table, columns, scenario, zoneRowidKeep(table)))
				_ = tx.Rollback()
				if err != nil {
					t.Fatalf("%s: an insert that leaves the rowid to SQLite: %v", which.name, err)
				}
			}
		})
	}
}

// The guard cannot tell an explicit rowid of -1 from the -1 a BEFORE INSERT trigger reads for an assigned one, so a row
// may be stored under -1, and the REPLACE of such a row is refused with the rowid's text while an assigned insert next to
// it is written: on an old zone that held rows under -1 before the open added the guards, and on a store the build created
// that is given one. A store whose tables hold no rowid -1 keeps no mark.
func TestDAGZoneRowidMinusOneIsWrittenAndItsReplaceIsRefused(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	oldDB := zoneKeyDB(t, old)
	for _, table := range zoneKeyGuardTables() {
		clearTable(t, oldDB, table)
		zoneMustExec(t, oldDB, zoneNthRow("INSERT", table, zoneTableColumns(t, oldDB, table), 0, "-1"))
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}
	zoneOpenClose(t, old) // the guards are added to the store that holds the -1 rows
	db := zoneKeyDB(t, current)
	for _, table := range zoneKeyGuardTables() {
		columns := zoneTableColumns(t, db, table)
		clearTable(t, db, table)
		zoneMustExec(t, db, zoneNthRow("INSERT", table, columns, 0, ""), zoneNthRow("INSERT", table, columns, 1, ""))
		if marks := zoneStringList(t, db, "SELECT table_name FROM dag_zone_rowid_marks"); len(marks) != 0 {
			t.Fatalf("%s: marks %v in a store whose tables hold no rowid -1", table, marks)
		}
		zoneMustExec(t, db, zoneNthRow("INSERT", table, columns, 2, "-1"))
	}
	for _, which := range []struct{ name, path string }{{"upgraded", old}, {"created", current}} {
		db := zoneKeyDB(t, which.path)
		for _, table := range zoneKeyGuardTables() {
			columns := zoneTableColumns(t, db, table)
			before := zoneTableContents(t, db, table)
			for _, verb := range []string{"INSERT OR REPLACE", "INSERT", "REPLACE"} {
				for _, name := range []string{"rowid", "_rowid_", "oid"} {
					statement := strings.Replace(zoneNthRow(verb, table, columns, 9, "-1"), "INTO "+table+" (rowid,", "INTO "+table+" ("+name+",", 1)
					err := zoneExec(t, db, statement)
					if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".rowid") {
						t.Errorf("%s: %s: %s of the stored rowid -1: %v; want the refusal of the rowid", which.name, table, verb, err)
					}
				}
			}
			// two REPLACEs in one statement: the second row's mark is its own
			err := zoneExec(t, db, zoneNthRow("INSERT OR REPLACE", table, columns, 8, "500")+", "+
				strings.SplitN(zoneNthRow("INSERT", table, columns, 9, "-1"), "VALUES ", 2)[1])
			if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".rowid") {
				t.Errorf("%s: %s: a free row and a REPLACE of rowid -1 in one statement: %v; want the refusal of the rowid", which.name, table, err)
			}
			if err := zoneExec(t, db, "INSERT OR IGNORE"+strings.TrimPrefix(zoneNthRow("INSERT", table, columns, 9, "-1"), "INSERT")); err != nil {
				t.Errorf("%s: %s: INSERT OR IGNORE of the stored rowid -1: %v; want a no-op", which.name, table, err)
			}
			if after := zoneTableContents(t, db, table); !slices.Equal(before, after) {
				t.Errorf("%s: %s: a refused row of rowid -1 changed the table:\n%q\n%q", which.name, table, before, after)
			}
			if err := zoneExec(t, db, zoneNthRow("INSERT", table, columns, 3, "")); err != nil {
				t.Errorf("%s: %s: an assigned insert next to a stored rowid of -1: %v", which.name, table, err)
			}
		}
	}
}

// zoneNthRow is an INSERT of a row of table that shares no key with the row of any other n: every key column carries
// n, except the columns the table's own BEFORE INSERT trigger pins (a revision with no parent, a live node, an outbox
// entry that opens a stream of its own). rowid, when not empty, is named with the given value.
func zoneNthRow(verb, table string, columns []zoneColumn, n int, rowid string) string {
	pinned := map[string]string{"parent_revision_no": "0", "retired_rev": "NULL", "state": "'pending'", "attempts": "0", "plan_revision": "1"}
	if table == "dag_summary_outbox" {
		pinned["seq"] = "1"
	}
	keyish := zoneKeyColumns(table)
	var names, values []string
	if rowid != "" {
		names, values = append(names, "rowid"), append(values, rowid)
	}
	for _, c := range columns {
		v, pin := pinned[c.name]
		key := slices.Contains(keyish, c.name)
		switch {
		case pin && (key || c.notnull):
		case key && c.integer:
			v = fmt.Sprint(100 + n)
		case key:
			v = fmt.Sprintf("'n%d-%s'", n, c.name)
		case c.notnull && !c.dflt && c.integer:
			v = "1"
		case c.notnull && !c.dflt:
			v = "'b-" + c.name + "'"
		default:
			continue
		}
		names, values = append(names, c.name), append(values, v)
	}
	return fmt.Sprintf("%s INTO %s (%s) VALUES (%s)", verb, table, strings.Join(names, ","), strings.Join(values, ","))
}

// zoneRowids is the rowids of table in order.
func zoneRowids(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return zoneStringList(t, db, "SELECT CAST(rowid AS TEXT) FROM "+table+" ORDER BY rowid")
}

// A table whose largest rowid is -2 has -1 assigned to its next row. That row is written and committed, and so is the
// one after it (0), on a store the build created and wrote a rowid of -2 into, and on an old zone that held a row
// under -2 before the open added the guards; a REPLACE of the row stored under -1 is still refused afterwards.
func TestDAGZoneAssignedInsertsFollowARowidOfMinusTwo(t *testing.T) {
	t.Parallel()
	current, old := zoneKeyStores(t)
	oldDB := zoneKeyDB(t, old)
	for _, table := range zoneKeyGuardTables() {
		clearTable(t, oldDB, table)
		zoneMustExec(t, oldDB, zoneNthRow("INSERT", table, zoneTableColumns(t, oldDB, table), 0, "-2"))
	}
	if err := oldDB.Close(); err != nil {
		t.Fatal(err)
	}
	zoneOpenClose(t, old) // the open adds the guards to the old zone that holds the rows under -2
	for _, which := range []struct{ name, path string }{{"created", current}, {"upgraded", old}} {
		db := zoneKeyDB(t, which.path)
		for _, table := range zoneKeyGuardTables() {
			columns := zoneTableColumns(t, db, table)
			if which.name == "created" {
				clearTable(t, db, table)
				zoneMustExec(t, db, zoneNthRow("INSERT", table, columns, 0, "-2"))
			}
			for n := 1; n <= 2; n++ {
				if err := zoneExec(t, db, zoneNthRow("INSERT", table, columns, n, "")); err != nil {
					t.Errorf("%s: %s: assigned insert %d after the rowid -2: %v", which.name, table, n, err)
				}
			}
			if got := zoneRowids(t, db, table); !slices.Equal(got, []string{"-2", "-1", "0"}) {
				t.Errorf("%s: %s: rowids %v, want [-2 -1 0] committed", which.name, table, got)
			}
			before := zoneTableContents(t, db, table)
			for _, rowid := range []string{"-1", "0", "-2"} {
				err := zoneExec(t, db, zoneNthRow("INSERT OR REPLACE", table, columns, 9, rowid))
				if err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".rowid") {
					t.Errorf("%s: %s: REPLACE of the stored rowid %s: %v; want the refusal of the rowid", which.name, table, rowid, err)
				}
			}
			if after := zoneTableContents(t, db, table); !slices.Equal(before, after) {
				t.Errorf("%s: %s: a refused REPLACE changed the table:\n%q\n%q", which.name, table, before, after)
			}
		}
	}
}
