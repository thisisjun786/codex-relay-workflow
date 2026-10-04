package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The additive DAG zone (docs/relay/dag-plans.md, decision D-01 of the DAG execution contract):
// tables the writable open creates AFTER it validated the frozen v1 tables, so a store that predates
// them still opens. These tests start from stores built without the zone, the shape every
// existing store has.

// zoneInventory is the expected column list of every zone table, written from the contract
// (4.5, 4.3, 2.2, 2.3, 6.2, 2.4) and not read back from the DDL under test.
var zoneInventory = map[string][]string{
	"dag_plans":                    {"plan_id", "project_key", "created_by_task_id", "created_at"},
	"dag_plan_revisions":           {"plan_id", "revision_no", "parent_revision_no", "request_id", "request_digest", "change_json", "state_digest", "coordinator_epoch", "author_task_id", "recorded_at"},
	"dag_nodes":                    {"plan_id", "node_id", "introduced_rev", "retired_rev", "slice_digest", "issue_key", "node_kind", "title", "criteria_set_digest", "supersedes_node_id"},
	"dag_edges":                    {"plan_id", "edge_id", "introduced_rev", "retired_rev", "from_node_id", "to_node_id", "kind", "target_repository", "target_base_ref", "pins_code_head", "decision_subject", "decision_digest", "required_authority"},
	"dag_input_manifests":          {"manifest_digest", "node_id", "body_json", "rule_version_json", "coordinator_epoch", "created_at"},
	"dag_node_executions":          {"plan_id", "node_id", "relationship_id", "execution_generation", "manifest_digest", "kind", "managed_request_id"},
	"dag_releases":                 {"plan_id", "node_id", "manifest_digest", "managed_request_id", "coordinator_epoch", "decided_at"},
	"dag_acceptances":              {"acceptance_id", "plan_id", "node_id", "manifest_digest", "relationship_id", "execution_generation", "event_id", "revision_hash", "criteria_set_digest", "verdict", "head_sha", "repository", "pr_number", "output_manifest_ref", "evidence_digest", "ack_tier", "verdict_turn_id", "rule_version_json", "accepted_by_task_id", "coordinator_epoch", "accepted_at", "supersedes_acceptance_id", "state"},
	"dag_integration_observations": {"observation_id", "acceptance_id", "repository", "base_ref", "subject_sha", "tip_sha", "is_ancestor", "method", "merge_turn_id", "observed_seq", "reverted_by", "observed_at"},
	"dag_decisions":                {"decision_id", "plan_id", "subject", "digest", "disposition", "authority_kind", "authority_ref", "revision", "state", "recorded_by_task_id", "coordinator_epoch", "recorded_at"},
	"dag_coordinator_claims":       {"plan_id", "epoch", "binding_id", "binding_revision", "task_id", "session_nonce", "claimed_at"},
	"dag_cap_basis":                {"limit_id", "limit_revision", "w_minutes", "w_source", "s_minutes", "s_source", "decided_by", "decided_at"},
	// CRW-184 (appended statements).
	"dag_merge_checks":             {"check_id", "acceptance_id", "check_seq", "head_sha", "observed_head_sha", "base_tip_sha", "checks_base_sha", "checks_digest", "evidence_json", "failed_required_json", "round_no", "outcome", "reason", "recorded_at"},
	"dag_acceptance_revalidations": {"revalidation_id", "acceptance_id", "criteria_set_digest", "event_id", "verdict_turn_id", "reval_seq", "revalidated_by", "revalidated_at"},
	"dag_acceptance_forge":         {"acceptance_id", "forge_repository", "pr_number"},
	"dag_passes":                   {"plan_id", "pass_seq", "plan_revision", "input_digest", "ready_count", "free_slots", "ceiling", "held", "deciding_limit", "order_json", "dispositions_json", "recorded_by", "recorded_at"},
	"dag_release_requests":         {"plan_id", "node_id", "manifest_digest", "request_sha256", "request_json", "marker_root", "socket", "state_selector", "recorded_at"},
	"dag_conflict_observations":    {"observation_id", "plan_id", "left_node_id", "right_node_id", "repository", "left_head", "right_head", "base_sha", "conflict_count", "method", "observed_by", "observed_at"},
	"dag_node_regions":             {"plan_id", "node_id", "declaration_seq", "repository", "path", "region_kind", "region_key", "change", "exclusive", "declared_by", "declared_at"},
	// CRW-282 (appended statements).
	"dag_release_recoveries": {"plan_id", "node_id", "manifest_digest", "abandoned_request_id", "action", "successor_request_id", "request_sha256", "request_json", "marker_root", "socket", "state_selector", "slot_id", "slot_released", "copy_path", "reason", "recorded_by", "coordinator_epoch", "recorded_at"},
	// CRW-283 (appended statements).
	"dag_summary_outbox": {"summary_id", "plan_id", "project_key", "document", "plan_revision", "seq", "subject_digest", "state_digest", "summary", "summary_sha256", "state", "attempts", "last_error", "claim_token", "claimed_by", "claimed_at", "readback", "confirmed_at", "enqueued_by", "coordinator_epoch", "created_at", "updated_at"},
	// CRW-409 (appended statements): the grade and rule of a declared region, and the files a conflict observation could not merge.
	"dag_node_region_grades":         {"plan_id", "node_id", "declaration_seq", "repository", "path", "region_kind", "region_key", "grade", "rule"},
	"dag_conflict_observation_files": {"observation_id", "repository", "path"},
	// CRW-410 (appended statements): head against tip, drift per node, the sweep ledger and its members.
	"dag_tip_conflict_observations":      {"observation_id", "plan_id", "node_id", "repository", "head", "head_source", "tip_ref", "tip_sha", "base_sha", "conflict_count", "method", "observed_by", "observed_at"},
	"dag_tip_conflict_observation_files": {"observation_id", "repository", "path"},
	"dag_conflict_drift":                 {"observation_id", "node_id", "path"},
	"dag_conflict_sweeps":                {"plan_id", "sweep_seq", "trigger_kind", "trigger_node", "trigger_ref", "repository", "observed_by", "observed_at"},
	"dag_conflict_sweep_members":         {"plan_id", "sweep_seq", "member_seq", "kind", "left_node_id", "right_node_id", "left_head", "right_head", "left_head_source", "right_head_source", "status", "reason", "observation_id", "conflicts"},
	// CRW-431 (appended statement): whether the declarer stated a whole-repository hold on a declared region.
	"dag_node_region_holds": {"plan_id", "node_id", "declaration_seq", "repository", "path", "region_kind", "region_key", "stated"},
	"dag_base_refreshes":    {"refresh_id", "acceptance_id", "refresh_seq", "relationship_id", "execution_generation", "event_id", "revision_hash", "head_sha", "base_repository", "base_ref", "base_tip_sha", "proof_json", "resolved_paths_json", "recorded_by_task_id", "coordinator_epoch", "recorded_at"},
	// CRW-446 (appended statement): the withdrawal of a generation that was opened by hand and never bound or sent.
	"dag_generation_withdrawals": {"relationship_id", "execution_generation", "plan_id", "node_id", "dispatch_request_id", "opened_reason", "restored_generation", "reason", "withdrawn_by_task_id", "coordinator_epoch", "withdrawn_at"},
	// CRW-411 (appended statements): the release policy of a plan, the results a parent records after a landing, and the policy state a recorded pass saw.
	"dag_release_policy":      {"plan_id", "policy_seq", "window_size", "handling_seconds", "red_merges", "clean_run", "recorded_by", "coordinator_epoch", "recorded_at"},
	"dag_landing_results":     {"result_id", "plan_id", "node_id", "kind", "commit_sha", "evidence", "recorded_by", "coordinator_epoch", "recorded_at"},
	"dag_pass_release_policy": {"plan_id", "pass_seq", "policy_json"},
	// CRW-468 (appended statement): the host memory bound a recorded pass saw.
	"dag_pass_host_memory": {"plan_id", "pass_seq", "state", "reading_limit", "host_json"},
}

// rawDB opens path without any of the store's open rules, as an operator's sqlite3 would.
func zoneRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// preDAGStore is a go-owned store built from the frozen v1 fixture (no dag_ object) and seeded with
// rows in several v1 tables, closed and checkpointed so the file alone holds it. It is a store of this
// version without the zone, so it holds the history indexes (testsupport.Create adds them): these tests
// are about the zone. The upgrade of a store that lacks the indexes is tested from
// testsupport.CreatePreviousVersion (history_index_test.go).
func zonePreDAGStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	testsupport.Create(t, path, "", "go")
	db := zoneRawDB(t, path)
	for _, statement := range []string{
		"INSERT INTO execution_limits VALUES ('lim-1','project','PRJ-A','runs','runs',6,1,'task-a','policy',1,'2026-10-01T00:00:00Z','2026-10-01T00:00:00Z')",
		"INSERT INTO scope_bindings VALUES ('bind-1','parent','project','PRJ-A','task-a','host-a',NULL,NULL,'active',1,NULL,NULL,NULL,'2026-10-01T00:00:00Z','2026-10-01T00:00:00Z')",
		"INSERT INTO scope_bindings VALUES ('bind-2','supervisor','initiative','INIT-1','task-s','host-s','/sup','cxc','active',3,NULL,NULL,'note','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	if _, err := db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	return path
}

func zoneCatalog(t *testing.T, path, where string) map[string]string {
	t.Helper()
	db := zoneRawDB(t, path)
	query := "SELECT type || ' ' || name, COALESCE(sql, '') FROM sqlite_master WHERE lower(substr(name,1,7)) <> 'sqlite_'"
	if where != "" {
		query += " AND (" + where + ")"
	}
	return zoneStringMap(t, db, query)
}

func zoneStringMap(t *testing.T, db *sql.DB, query string) map[string]string {
	t.Helper()
	rows, err := db.Query(query)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			t.Fatal(err)
		}
		out[key] = value
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func zoneOpenClose(t *testing.T, path string) {
	t.Helper()
	s, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}

func zoneTables(t *testing.T, path string) []string {
	t.Helper()
	var names []string
	for key := range zoneCatalog(t, path, "type='table' AND name LIKE 'dag\\_%' ESCAPE '\\'") {
		names = append(names, strings.TrimPrefix(key, "table "))
	}
	sort.Strings(names)
	return names
}

// A store built before the zone existed opens, keeps every row it held value for value, and gains
// every zone table (D-01: validate the frozen tables, then create the zone).
func TestDAGZonePreDAGStoreOpensAndKeepsItsRows(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	if got := zoneTables(t, path); len(got) != 0 {
		t.Fatalf("the fixture already holds zone tables: %v", got)
	}
	db := zoneRawDB(t, path)
	before := testsupport.TableRows(t, db, "")
	if len(before["scope_bindings"]) != 2 || len(before["execution_limits"]) != 1 {
		t.Fatalf("seed rows missing: %v", before)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	v1Before := zoneCatalog(t, path, "")

	zoneOpenClose(t, path)

	var want []string
	for table := range zoneInventory {
		want = append(want, table)
	}
	sort.Strings(want)
	if got := zoneTables(t, path); !reflect.DeepEqual(got, want) {
		t.Fatalf("zone tables after open = %v, want %v", got, want)
	}
	after := testsupport.TableRows(t, zoneRawDB(t, path), "name NOT LIKE 'dag\\_%' ESCAPE '\\'")
	// The one row the open adds to a store from before the marker is the marker of the settlements
	// backfill (the open ran it, and a store without observations has nothing to fill).
	want1 := map[string][]map[string]any{}
	for table, rows := range before {
		want1[table] = rows
	}
	want1["schema_meta"] = append(append([]map[string]any(nil), before["schema_meta"]...), map[string]any{"key": "backfill:assignment_settlements", "value": "1"})
	if !reflect.DeepEqual(after, want1) {
		t.Fatalf("v1 rows changed by the open beyond the backfill marker:\n before %v\n after  %v", before, after)
	}
	v1After := zoneCatalog(t, path, "name NOT LIKE 'dag\\_%' ESCAPE '\\' AND tbl_name NOT LIKE 'dag\\_%' ESCAPE '\\'")
	if !reflect.DeepEqual(v1After, v1Before) {
		t.Fatal("a v1 schema object changed (text or presence) when the zone was created")
	}
}

// Missing a table of the frozen v1 schema is still refused, and nothing of the zone is created
// on the way to the refusal.
func TestDAGZoneFrozenTableMissingIsStillRefused(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	if _, err := zoneRawDB(t, path).Exec("DROP TABLE attempt_messages"); err != nil {
		t.Fatal(err)
	}
	_, err := fixtureOpen(context.Background(), path, "")
	if err == nil || !strings.Contains(err.Error(), "required table missing: attempt_messages") {
		t.Fatalf("open = %v, want the refusal naming attempt_messages", err)
	}
	if got := zoneTables(t, path); len(got) != 0 {
		t.Fatalf("the refused open created zone tables: %v", got)
	}
}

// The open is idempotent: a second open changes nothing in the catalog.
func TestDAGZoneOpenIsIdempotent(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	first := zoneCatalog(t, path, "")
	zoneOpenClose(t, path)
	if second := zoneCatalog(t, path, ""); !reflect.DeepEqual(first, second) {
		t.Fatal("a second open changed the catalog")
	}
}

// Every zone table has exactly the columns the contract names, in order, and the files hold no
// zone table beyond them.
func TestDAGZoneInventory(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneRawDB(t, path)
	for table, want := range zoneInventory {
		rows, err := db.Query("SELECT name FROM pragma_table_info('" + table + "') ORDER BY cid")
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			got = append(got, name)
		}
		_ = rows.Close()
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s columns = %v, want %v", table, got, want)
		}
	}
	if got := zoneTables(t, path); len(got) != len(zoneInventory) {
		t.Errorf("zone tables = %v, want exactly %d", got, len(zoneInventory))
	}
}

// zoneShipped is the text of every zone object as it shipped (testdata/dag_zone_shipped.json): a
// shipped statement is never edited (dag_zone.go), because the swap gate compares this text.
func zoneShipped(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "dag_zone_shipped.json"))
	if err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Objects map[string]string `json:"objects"`
	}
	if err := json.Unmarshal(data, &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot.Objects
}

func zoneObjects(t *testing.T, path string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for key, text := range zoneCatalog(t, path, "name LIKE 'dag\\_%' ESCAPE '\\' OR tbl_name LIKE 'dag\\_%' ESCAPE '\\'") {
		out[key] = normalizeSQL(text)
	}
	return out
}

// Writing the snapshot is a deliberate act (CRW_GOLDEN=update), reviewed in the diff.
func TestDAGZoneShippedTextIsFrozen(t *testing.T) {
	// Serial: with CRW_GOLDEN=update it writes the fixed path testdata/dag_zone_shipped.json.
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	got := zoneObjects(t, path)
	if golden.Updating() {
		data, err := json.MarshalIndent(map[string]any{"objects": got}, "", "  ")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join("testdata", "dag_zone_shipped.json"), append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	shipped := zoneShipped(t)
	for key, held := range shipped {
		if got[key] != normalizeSQL(held) {
			t.Errorf("shipped zone object changed or went missing: %s", key)
		}
	}
	for key := range got {
		if _, ok := shipped[key]; !ok {
			t.Errorf("zone object %s is not in testdata/dag_zone_shipped.json: record it (CRW_GOLDEN=update) when a statement is appended", key)
		}
	}
}

// A store the build created fresh and a pre-zone store the build upgraded read the same zone text.
func TestDAGZoneFreshEqualsUpgraded(t *testing.T) {
	t.Parallel()
	upgraded := zonePreDAGStore(t)
	zoneOpenClose(t, upgraded)
	fresh := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := fixtureOpen(context.Background(), fresh, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got, want := zoneObjects(t, fresh), zoneObjects(t, upgraded); !reflect.DeepEqual(got, want) {
		t.Fatal("the zone of a fresh store and of an upgraded store differ")
	}
}

// What a runtime without the zone does at open: validate the frozen tables and run the v1 script. Both
// still succeed on a store that carries the zone, and neither touches a zone row.
func TestDAGZoneOlderRuntimeStillOpensAStoreWithTheZone(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneRawDB(t, path)
	for _, statement := range []string{
		"INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-02T00:00:00Z')",
		"INSERT INTO dag_cap_basis VALUES ('lim-1',1,90,'record 1',20,'record 2','task-a','2026-10-02T00:00:00Z')",
	} {
		if _, err := db.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	zoneBefore := testsupport.TableRows(t, db, "name LIKE 'dag\\_%' ESCAPE '\\'")
	v1Before := testsupport.TableRows(t, db, "name NOT LIKE 'dag\\_%' ESCAPE '\\'")
	ctx := context.Background()
	if err := ValidateOwnershipSchema(ctx, db); err != nil {
		t.Fatalf("the frozen-table validation refuses a store that carries the zone: %v", err)
	}
	ddl, guards, err := SchemaStatements()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(ddl); err != nil {
		t.Fatalf("the v1 script fails on a store that carries the zone: %v", err)
	}
	for _, guard := range guards {
		if _, err := db.Exec(guard); err != nil {
			t.Fatalf("a v1 guard index fails on a store that carries the zone: %v", err)
		}
	}
	if got := testsupport.TableRows(t, db, "name LIKE 'dag\\_%' ESCAPE '\\'"); !reflect.DeepEqual(got, zoneBefore) {
		t.Fatal("the v1 script changed zone rows")
	}
	if got := testsupport.TableRows(t, db, "name NOT LIKE 'dag\\_%' ESCAPE '\\'"); !reflect.DeepEqual(got, v1Before) {
		t.Fatal("the v1 script changed v1 rows")
	}
}

// A read-only open creates nothing: a store without the zone stays without it.
func TestDAGZoneReadOnlyOpenCreatesNothing(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	s, err := OpenReadOnlyStore(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if got := zoneTables(t, path); len(got) != 0 {
		t.Fatalf("a read-only open created %v", got)
	}
}

func zoneExec(t *testing.T, db *sql.DB, statement string) error {
	t.Helper()
	_, err := db.Exec(statement)
	return err
}

func zoneMustExec(t *testing.T, db *sql.DB, statements ...string) {
	t.Helper()
	for _, statement := range statements {
		if err := zoneExec(t, db, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
}

func zoneRefuses(t *testing.T, db *sql.DB, why, statement string) {
	t.Helper()
	if err := zoneExec(t, db, statement); err == nil {
		t.Errorf("%s: accepted: %s", why, statement)
	}
}

func zoneOpenedDB(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	s, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s.DB
}

const zoneDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

// The log cannot be rewritten or branched, whoever writes the row: UPDATE and DELETE abort, a revision
// needs its parent, and revision n has parent n-1.
func TestDAGZoneRevisionLogIsAppendOnly(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	rev := func(plan string, no, parent int, request string) string {
		return fmt.Sprintf("INSERT INTO dag_plan_revisions VALUES ('%s',%d,%d,'%s','%s','[]','%s',0,'task-a','2026-10-02T00:00:00Z')", plan, no, parent, request, zoneDigest, zoneDigest)
	}
	zoneRefuses(t, db, "a revision of a plan that was never created", rev("plan-1", 1, 0, "r1"))
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-02T00:00:00Z')", rev("plan-1", 1, 0, "r1"))
	zoneRefuses(t, db, "a second first revision", rev("plan-1", 1, 0, "r1b"))
	zoneRefuses(t, db, "a revision whose parent is not the previous one", rev("plan-1", 3, 1, "r3"))
	zoneRefuses(t, db, "a revision whose parent does not exist", rev("plan-1", 3, 2, "r3"))
	zoneRefuses(t, db, "a repeated request id", rev("plan-1", 2, 1, "r1"))
	zoneMustExec(t, db, rev("plan-1", 2, 1, "r2"))
	zoneRefuses(t, db, "an UPDATE of a revision", "UPDATE dag_plan_revisions SET change_json = '[1]' WHERE revision_no = 1")
	zoneRefuses(t, db, "a DELETE of a revision", "DELETE FROM dag_plan_revisions WHERE revision_no = 2")
	zoneRefuses(t, db, "an UPDATE of a plan", "UPDATE dag_plans SET project_key = 'X'")
	zoneRefuses(t, db, "a DELETE of a plan", "DELETE FROM dag_plans")
	var n int
	if err := db.QueryRow("SELECT count(*) FROM dag_plan_revisions").Scan(&n); err != nil || n != 2 {
		t.Fatalf("revisions = %d (%v), want 2", n, err)
	}
}

// A node or edge row may be retired once and nothing else about it may change; it is never deleted.
func TestDAGZoneFoldRowsAreRetireOnly(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-02T00:00:00Z')")
	for no := 1; no <= 3; no++ {
		zoneMustExec(t, db, fmt.Sprintf("INSERT INTO dag_plan_revisions VALUES ('plan-1',%d,%d,'r%d','%s','[]','%s',0,'task-a','2026-10-02T00:00:00Z')", no, no-1, no, zoneDigest, zoneDigest))
	}
	node := func(id string, intro int) string {
		return fmt.Sprintf("INSERT INTO dag_nodes VALUES ('plan-1','%s',%d,NULL,'%s','CRW-1','implementation',NULL,'%s',NULL)", id, intro, zoneDigest, zoneDigest)
	}
	zoneMustExec(t, db, node("a", 1), node("b", 1))
	zoneRefuses(t, db, "two live versions of one node", node("a", 2))
	zoneRefuses(t, db, "a node introduced at a revision that does not exist", node("c", 9))
	zoneRefuses(t, db, "an edit of a node's digest", "UPDATE dag_nodes SET slice_digest = 'x' WHERE node_id = 'a'")
	zoneRefuses(t, db, "a retirement that also edits", "UPDATE dag_nodes SET retired_rev = 2, issue_key = 'CRW-2' WHERE node_id = 'a'")
	zoneRefuses(t, db, "a retirement at or before introduction", "UPDATE dag_nodes SET retired_rev = 1 WHERE node_id = 'a'")
	zoneRefuses(t, db, "a node's deletion", "DELETE FROM dag_nodes WHERE node_id = 'b'")
	zoneMustExec(t, db, "UPDATE dag_nodes SET retired_rev = 2 WHERE node_id = 'a'", node("a", 2))
	zoneRefuses(t, db, "retiring a retired node again", "UPDATE dag_nodes SET retired_rev = 3 WHERE node_id = 'a' AND introduced_rev = 1")
	edge := "INSERT INTO dag_edges VALUES ('plan-1','e1',1,NULL,'a','b','artifact_verified',NULL,NULL,0,NULL,NULL,NULL)"
	zoneMustExec(t, db, edge)
	zoneRefuses(t, db, "an edit of an edge", "UPDATE dag_edges SET to_node_id = 'a' WHERE edge_id = 'e1'")
	zoneRefuses(t, db, "an edge's deletion", "DELETE FROM dag_edges WHERE edge_id = 'e1'")
	zoneMustExec(t, db, "UPDATE dag_edges SET retired_rev = 2 WHERE edge_id = 'e1'")
}

// The contract 2.4 rejections hold at the table too, so a bug in a validator cannot store such an edge.
func TestDAGZoneEdgeIntegrityChecks(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-02T00:00:00Z')",
		fmt.Sprintf("INSERT INTO dag_plan_revisions VALUES ('plan-1',1,0,'r1','%s','[]','%s',0,'task-a','2026-10-02T00:00:00Z')", zoneDigest, zoneDigest))
	edge := func(kind, repo, base string, pins int, subject, digest, authority string) string {
		quote := func(v string) string {
			if v == "" {
				return "NULL"
			}
			return "'" + v + "'"
		}
		return fmt.Sprintf("INSERT INTO dag_edges VALUES ('plan-1','e-%s-%d',1,NULL,'a','b','%s',%s,%s,%d,%s,%s,%s)",
			kind, len(repo)+len(subject)+len(authority)+pins, kind, quote(repo), quote(base), pins, quote(subject), quote(digest), quote(authority))
	}
	zoneRefuses(t, db, "integrated without a target", edge("integrated", "", "", 0, "", "", ""))
	zoneRefuses(t, db, "integrated without a base ref", edge("integrated", "o/r", "", 0, "", "", ""))
	zoneRefuses(t, db, "a code pin without a target", edge("artifact_verified", "", "", 1, "", "", ""))
	zoneRefuses(t, db, "a decision without its fields", edge("decision", "", "", 0, "", "", ""))
	zoneRefuses(t, db, "a decision with an empty authority", edge("decision", "", "", 0, "s", zoneDigest, "[]"))
	zoneRefuses(t, db, "an unknown edge kind", edge("blocks", "", "", 0, "", "", ""))
	zoneRefuses(t, db, "a self-referencing edge", "INSERT INTO dag_edges VALUES ('plan-1','e-self',1,NULL,'a','a','artifact_verified',NULL,NULL,0,NULL,NULL,NULL)")
	zoneMustExec(t, db, edge("integrated", "o/r", "dev", 0, "", "", ""),
		edge("artifact_verified", "o/r", "dev", 1, "", "", ""),
		edge("decision", "", "", 0, "subject", zoneDigest, `["owner"]`))
}

// Node ids are plan-local: two plans may use the same ids, and every uniqueness stays inside its plan.
func TestDAGZoneTwoPlansMayUseTheSameNodeIDs(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	for _, plan := range []string{"plan-1", "plan-2"} {
		zoneMustExec(t, db,
			fmt.Sprintf("INSERT INTO dag_plans VALUES ('%s','PRJ-A','task-a','2026-10-02T00:00:00Z')", plan),
			fmt.Sprintf("INSERT INTO dag_plan_revisions VALUES ('%s',1,0,'r1','%s','[]','%s',0,'task-a','2026-10-02T00:00:00Z')", plan, zoneDigest, zoneDigest),
			fmt.Sprintf("INSERT INTO dag_nodes VALUES ('%s','a',1,NULL,'%s','CRW-1','non_pr',NULL,'%s',NULL)", plan, zoneDigest, zoneDigest),
			fmt.Sprintf("INSERT INTO dag_releases VALUES ('%s','a','%s','req-%s',1,'2026-10-02T00:00:00Z')", plan, zoneDigest, plan),
			fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-%s','%s','a','%s','rel-%s',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-02T00:00:00Z',NULL,'active')", plan, plan, zoneDigest, plan, zoneDigest, zoneDigest),
			fmt.Sprintf("INSERT INTO dag_decisions VALUES ('dec-%s','%s','subject','%s','approved','user','ref',1,'active','task-a',1,'2026-10-02T00:00:00Z')", plan, plan, zoneDigest),
			fmt.Sprintf("INSERT INTO dag_coordinator_claims VALUES ('%s',1,'bind','1','task-a','nonce','2026-10-02T00:00:00Z')", plan))
	}
	zoneRefuses(t, db, "a second release of one node and manifest in a plan", fmt.Sprintf("INSERT INTO dag_releases VALUES ('plan-1','a','%s','req-x',1,'2026-10-02T00:00:00Z')", zoneDigest))
	zoneRefuses(t, db, "a second active acceptance of a node in a plan", fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-x','plan-1','a','%s','rel-x',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-02T00:00:00Z',NULL,'active')", zoneDigest, zoneDigest, zoneDigest))
	zoneRefuses(t, db, "an acceptance on an unverified ack", fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-y','plan-1','b','%s','rel-y',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'unverified','turn','{}','task-a',1,'2026-10-02T00:00:00Z',NULL,'superseded')", zoneDigest, zoneDigest, zoneDigest))
	zoneRefuses(t, db, "a second active decision of a subject in a plan", fmt.Sprintf("INSERT INTO dag_decisions VALUES ('dec-x','plan-1','subject','%s','approved','user','ref',2,'active','task-a',1,'2026-10-02T00:00:00Z')", zoneDigest))
	zoneRefuses(t, db, "a second claim of one epoch in a plan", "INSERT INTO dag_coordinator_claims VALUES ('plan-1',1,'bind','1','task-b','nonce','2026-10-02T00:00:00Z')")
}

// A command that declares itself read-only leaves the schema alone: the zone arrives with the first write.
func TestDAGZoneReadOnlyCommandDoesNotCreateIt(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	before := zoneCatalog(t, path, "")
	s, err := Open(WithReadOnlyCommand(context.Background()), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if after := zoneCatalog(t, path, ""); !reflect.DeepEqual(after, before) {
		t.Fatal("a read-only command changed the schema")
	}
	zoneOpenClose(t, path)
	if got := zoneTables(t, path); len(got) != len(zoneInventory) {
		t.Fatalf("the first write open created %v", got)
	}
}

// pendingWriters are the zone tables whose first writer is a later issue of the DAG project: no production query names them
// yet. The list is exactly those tables: a table gains a query and leaves this list in the same change, and a table with neither
// is dead schema.
var pendingWriters = map[string]string{}

func TestDAGZoneEveryTableHasAQueryOrAPendingWriter(t *testing.T) {
	t.Parallel()
	var tables []string
	for table := range zoneInventory {
		tables = append(tables, table)
	}
	sort.Strings(tables)
	unreferenced := map[string]bool{}
	for _, table := range unreferencedTables(tables, productionSQL(t, "\x00")) {
		unreferenced[table] = true
	}
	for _, table := range tables {
		_, pending := pendingWriters[table]
		switch {
		case unreferenced[table] && !pending:
			t.Errorf("%s has no production query and no pending writer: dead schema", table)
		case !unreferenced[table] && pending:
			t.Errorf("%s has a production query now: remove it from pendingWriters", table)
		}
	}
	for table := range pendingWriters {
		if _, ok := zoneInventory[table]; !ok {
			t.Errorf("pendingWriters names %s, which is not a zone table", table)
		}
	}
}

// The CRW-184 tables carry their own constraints in the table, so a scheduler bug cannot store a row they refuse: the merge-check and
// revalidation histories are ordered (one row per sequence number), the closed vocabularies are CHECKs, and each dependent row needs
// its acceptance (foreign keys are on for every connection).
func TestDAGZoneSchedulerTablesRefuseBadRows(t *testing.T) {
	t.Parallel()
	db := zoneOpenedDB(t)
	zoneMustExec(t, db, fmt.Sprintf("INSERT INTO dag_acceptances VALUES ('acc-1','plan-1','a','%s','rel-1',1,'evt','%s','%s','verified',NULL,NULL,NULL,NULL,NULL,'host','turn','{}','task-a',1,'2026-10-02T00:00:00Z',NULL,'active')", zoneDigest, zoneDigest, zoneDigest))
	check := func(id string, seq int, outcome string, round int) string {
		return fmt.Sprintf("INSERT INTO dag_merge_checks VALUES ('%s','acc-1',%d,'h1','h1','tip',NULL,'%s','{}','[]',%d,'%s','why','2026-10-02T00:00:00Z')", id, seq, zoneDigest, round, outcome)
	}
	zoneMustExec(t, db, check("c1", 1, "eligible", 1), check("c2", 2, "stale_base", 1), check("c3", 3, "eligible", 1))
	zoneRefuses(t, db, "a second row with the same sequence number", check("c4", 3, "evicted", 2))
	zoneRefuses(t, db, "an outcome outside the vocabulary", check("c5", 4, "merged", 1))
	zoneRefuses(t, db, "a third retry round", check("c6", 4, "evicted", 3))
	zoneRefuses(t, db, "a merge check of an acceptance that does not exist", strings.Replace(check("c7", 1, "eligible", 1), "'acc-1'", "'acc-none'", 1))
	reval := func(id string, seq int, digest string) string {
		return fmt.Sprintf("INSERT INTO dag_acceptance_revalidations VALUES ('%s','acc-1','%s','evt','turn',%d,'task-a','2026-10-02T00:00:00Z')", id, digest, seq)
	}
	other := strings.Repeat("b", 64)
	// criteria B, then C, then B again: three rows, each its own sequence number (a UNIQUE on the digest would refuse the third).
	zoneMustExec(t, db, reval("r1", 1, zoneDigest), reval("r2", 2, other), reval("r3", 3, zoneDigest))
	zoneRefuses(t, db, "a second revalidation with the same sequence number", reval("r4", 3, other))
	zoneRefuses(t, db, "a revalidation of an acceptance that does not exist", strings.Replace(reval("r5", 1, other), "'acc-1'", "'acc-none'", 1))
	forge := func(acceptance, repo string, pr int) string {
		return fmt.Sprintf("INSERT INTO dag_acceptance_forge VALUES ('%s','%s',%d)", acceptance, repo, pr)
	}
	zoneRefuses(t, db, "a forge row before its acceptance (the order Accept must not use)", forge("acc-none", "o/r", 7))
	zoneRefuses(t, db, "a pull request number of zero", forge("acc-1", "o/r", 0))
	zoneRefuses(t, db, "an empty forge repository", forge("acc-1", "", 7))
	zoneMustExec(t, db, forge("acc-1", "o/r", 7))
	zoneRefuses(t, db, "a second forge identity for one acceptance", forge("acc-1", "o/r", 8))
}
