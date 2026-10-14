package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"
)

// CRW-1212 (R04): the flow journal tables (flow_runs, flow_calls, flow_events, flow_spawn_decisions, flow_step_decisions) and the
// agent execution rows (dag_agent_executions, dag_agent_closures), appended to the zone ledger. Each table is append-only (UPDATE and
// DELETE abort), refuses a NULL key (CRW-835) and a REPLACE of a stored key or rowid (CRW-974), and an old zone that lacks them is
// extended by the open that finds them missing, keeping every row it holds. These tests write the tables directly, as an operator's
// sqlite3 would, on a store the build opened (foreign keys on, as every connection of the store has them).

var (
	flowTables  = []string{"flow_runs", "flow_calls", "flow_events", "flow_spawn_decisions", "flow_step_decisions"}
	agentTables = []string{"dag_agent_executions", "dag_agent_closures"}
	// CRW-1213: the agent acceptance chain and the node-level stages of an agent acceptance, appended after the agent execution tables.
	acceptTables = []string{"dag_agent_acceptances", "dag_agent_integration_stages"}
)

func flowRunRow(id string) string {
	return fmt.Sprintf("INSERT INTO flow_runs (run_id, workflow, script_digest, helper_digests_json, args_digest, plan_id, plan_revision, actor_task_id, epoch_nonce, resume_from, started_at)"+
		" VALUES (%s,'project-lane','%s','{}','%s','plan-1',1,'task-orchestrator','nonce-1','','2026-10-11T00:00:00Z')", sqlText(id), zoneDigest, zoneDigest)
}

// flowCallRow is the INSERT of one call event; call and seq are SQL literals so a test can write NULL.
func flowCallRow(call, seq, run, event string) string {
	return fmt.Sprintf("INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, role, event, commit_sha, commit_parent, tree_digest, verification_digest, acceptance_id, batch_id, recorded_at)"+
		" VALUES (%s,%s,%s,'build:n1',0,'agent','executor',%s,'c1','c0','t1','v1','a1','b1','2026-10-11T00:00:01Z')", call, seq, run, event)
}

func flowEventRow(run, seq string) string {
	return fmt.Sprintf("INSERT INTO flow_events (run_id, seq, kind, payload_json, recorded_at) VALUES (%s,%s,'phase','{}','2026-10-11T00:00:02Z')", run, seq)
}

func flowSpawnRow(id, run string) string {
	return fmt.Sprintf("INSERT INTO flow_spawn_decisions (decision_id, run_id, kind, decision, evidence_json, decided_at) VALUES (%s,%s,'spawn','spawn','[]','2026-10-11T00:00:03Z')", id, run)
}

func flowStepRow(id, run string) string {
	return fmt.Sprintf("INSERT INTO flow_step_decisions (step_id, run_id, node_id, from_tier, to_tier, rule_id, evidence_json, decided_at) VALUES (%s,%s,'n1','executor','executor-t1','SP-TIER-2','[]','2026-10-11T00:00:04Z')", id, run)
}

func sqlText(v string) string { return "'" + strings.ReplaceAll(v, "'", "''") + "'" }

// flowRows is one valid row per flow table, in an order a foreign key allows, and the statement of each that repeats its key.
func flowRows() (seed []string, byTable map[string]string) {
	byTable = map[string]string{
		"flow_runs":            flowRunRow("run-1"),
		"flow_calls":           flowCallRow("'call-1'", "1", "'run-1'", "'prepared'"),
		"flow_events":          flowEventRow("'run-1'", "1"),
		"flow_spawn_decisions": flowSpawnRow("'spawn-1'", "'run-1'"),
		"flow_step_decisions":  flowStepRow("'step-1'", "'run-1'"),
	}
	for _, table := range flowTables {
		seed = append(seed, byTable[table])
	}
	return seed, byTable
}

// openedZone is a store the build opened, with the foreign keys on.
func openedZone(t *testing.T) *sql.DB {
	t.Helper()
	return zoneOpenedDB(t)
}

func agentSeed(t *testing.T, db *sql.DB) {
	t.Helper()
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-1','PRJ-A','task-a','2026-10-11T00:00:00Z')",
		fmt.Sprintf("INSERT INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES ('%s','n1','{}','{}',0,'2026-10-11T00:00:00Z')", zoneDigest),
		fmt.Sprintf("INSERT INTO dag_input_manifests (manifest_digest, node_id, body_json, rule_version_json, coordinator_epoch, created_at) VALUES ('%s','n1','{\"v\":2}','{}',0,'2026-10-11T00:00:00Z')", strings.Repeat("b", 64)))
}

// agentExecRow is the INSERT of one execution; id, node, kind and supersedes are SQL literals. The attempt of a row that supersedes another is the attempt of that one plus one (a supersedes that
// names no row leaves it NULL, and the row is refused all the same).
func agentExecRow(id, node, kind, supersedes string) string {
	attempt := "1"
	if supersedes != "NULL" {
		attempt = "(SELECT attempt + 1 FROM dag_agent_executions WHERE execution_id = " + supersedes + ")"
	}
	return agentExecRowAt(id, "'plan-1'", node, attempt, kind, supersedes)
}

// agentExecRowAt is the INSERT of one execution with its plan and attempt given as SQL as well.
func agentExecRowAt(id, plan, node, attempt, kind, supersedes string) string {
	return fmt.Sprintf("INSERT INTO dag_agent_executions (execution_id, plan_id, node_id, attempt, kind, manifest_digest, role, tier, actor_task_id, coordinator_epoch, decided_at, supersedes_execution_id)"+
		" VALUES (%s,%s,%s,%s,%s,'%s','executor','executor','task-orchestrator',1,'2026-10-11T00:00:05Z',%s)", id, plan, node, attempt, kind, zoneDigest, supersedes)
}

func agentCloseRow(kind, id, state string) string {
	return fmt.Sprintf("INSERT INTO dag_agent_closures (subject_kind, subject_id, state, slot_released, actor_task_id, coordinator_epoch, reason, closed_at) VALUES (%s,%s,%s,1,'task-orchestrator',1,'cancelled by the run','2026-10-11T00:00:06Z')", kind, id, state)
}

func zoneRows(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	return zoneTableContents(t, db, table)
}

// A flow table row is never edited or removed, whoever writes it: UPDATE and DELETE abort with the table's own text, the row reads as it was,
// and a row with a later event is a row added.
func TestFlowZoneAppendOnly(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	seed, _ := flowRows()
	zoneMustExec(t, db, seed...)
	zoneMustExec(t, db, flowCallRow("'call-1'", "2", "'run-1'", "'thread_requested'"), flowEventRow("'run-1'", "2"))
	for _, table := range flowTables {
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
			t.Errorf("%s changed over refused edits:\n%q\n%q", table, before, after)
		}
	}
	// the foreign keys of the journal point at flow_runs: a call of a run that does not exist is refused
	zoneRefuses(t, db, "a call of an unknown run", flowCallRow("'call-9'", "1", "'run-none'", "'prepared'"))
	zoneRefuses(t, db, "an event of an unknown run", flowEventRow("'run-none'", "1"))
	zoneRefuses(t, db, "an event sequence of zero", flowEventRow("'run-1'", "0"))
	zoneRefuses(t, db, "a worktree kind outside the vocabulary", "INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, event, worktree_kind, recorded_at) VALUES ('call-2',1,'run-1','p',0,'agent','prepared','elsewhere','2026-10-11T00:00:01Z')")
	zoneMustExec(t, db, "INSERT INTO flow_calls (call_id, event_seq, run_id, path, position, kind, event, worktree_kind, recorded_at) VALUES ('call-2',1,'run-1','p',0,'agent','prepared','restore','2026-10-11T00:00:01Z')")
}

// A key column refuses NULL with the text of CRW-835, in every flow table and in either column of a composite key; nothing is written.
func TestFlowZoneNullGuards(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	zoneMustExec(t, db, flowRunRow("run-1"))
	cases := []struct{ table, key, statement string }{
		{"flow_runs", "run_id", strings.Replace(flowRunRow("x"), "VALUES ('x'", "VALUES (NULL", 1)},
		{"flow_calls", "call_id", flowCallRow("NULL", "1", "'run-1'", "'prepared'")},
		{"flow_calls", "call_id", flowCallRow("'call-1'", "NULL", "'run-1'", "'prepared'")},
		{"flow_events", "run_id", flowEventRow("NULL", "1")},
		{"flow_events", "run_id", flowEventRow("'run-1'", "NULL")},
		{"flow_spawn_decisions", "decision_id", flowSpawnRow("NULL", "'run-1'")},
		{"flow_step_decisions", "step_id", flowStepRow("NULL", "'run-1'")},
	}
	for _, c := range cases {
		err := zoneExec(t, db, c.statement)
		want := c.table + "." + c.key + " is NULL: a row is addressed by a non-empty id"
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %s: %v; want %q", c.table, c.statement, err, want)
		}
	}
	for _, table := range flowTables[1:] {
		if rows := zoneRows(t, db, table); len(rows) != 0 {
			t.Errorf("%s holds %v after refused NULL keys", table, rows)
		}
	}
	// the empty key keeps the CHECK of its column
	zoneRefuses(t, db, "an empty run id", flowRunRow(""))
}

// A flow table refuses an INSERT OR REPLACE that repeats a stored key or names a stored rowid (CRW-974): the row stays as it was, and a plain
// INSERT of the same key gets the text SQLite gives it.
func TestFlowZoneNoReplace(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	seed, byTable := flowRows()
	zoneMustExec(t, db, seed...)
	keys := map[string]string{
		"flow_runs":            "flow_runs.run_id",
		"flow_calls":           "flow_calls.call_id, flow_calls.event_seq",
		"flow_events":          "flow_events.run_id, flow_events.seq",
		"flow_spawn_decisions": "flow_spawn_decisions.decision_id",
		"flow_step_decisions":  "flow_step_decisions.step_id",
	}
	for _, table := range flowTables {
		before := zoneRows(t, db, table)
		repeat := byTable[table]
		for _, verb := range []string{"INSERT OR REPLACE INTO", "INSERT INTO", "REPLACE INTO"} {
			statement := strings.Replace(repeat, "INSERT INTO", verb, 1)
			// the repeated row differs from the stored one in a column that is not part of its key
			statement = strings.Replace(statement, "2026-10-11T0", "2026-10-12T0", 1)
			if err := zoneExec(t, db, statement); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+keys[table]) {
				t.Errorf("%s: %s: %v; want the refusal of %s", table, verb, err, keys[table])
			}
		}
		var rowid int64
		if err := db.QueryRow("SELECT rowid FROM " + table).Scan(&rowid); err != nil {
			t.Fatal(err)
		}
		// a row with a free key that names the rowid of the stored row replaces it on an unguarded table
		free := freeKeyRow(table, byTable)
		replace := strings.Replace(free, "INSERT INTO "+table+" (", fmt.Sprintf("INSERT OR REPLACE INTO %s (rowid, ", table), 1)
		replace = strings.Replace(replace, "VALUES (", fmt.Sprintf("VALUES (%d, ", rowid), 1)
		if err := zoneExec(t, db, replace); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".rowid") {
			t.Errorf("%s: REPLACE of the stored rowid: %v", table, err)
		}
		if after := zoneRows(t, db, table); !slices.Equal(before, after) {
			t.Errorf("%s changed over refused REPLACEs:\n%q\n%q", table, before, after)
		}
	}
}

// freeKeyRow is the seed row of table with a key no seed row holds.
func freeKeyRow(table string, byTable map[string]string) string {
	switch table {
	case "flow_runs":
		return flowRunRow("run-free")
	case "flow_calls":
		return flowCallRow("'call-free'", "1", "'run-1'", "'prepared'")
	case "flow_events":
		return flowEventRow("'run-1'", "99")
	case "flow_spawn_decisions":
		return flowSpawnRow("'spawn-free'", "'run-1'")
	}
	return flowStepRow("'step-free'", "'run-1'")
}

// zoneWithout is a store the build opened with the tables dropped: the zone ledger an earlier build left, which does not know them. The
// dropped tables take their indexes and triggers with them.
func zoneWithout(t *testing.T, tables []string) string {
	t.Helper()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	db := zoneRawDB(t, path)
	for _, table := range slices.Backward(tables) {
		zoneMustExec(t, db, "DROP TABLE "+table)
	}
	return path
}

// upgradeCheck: the open of the build adds the tables and every guard to the old zone in one step, keeps the rows the old zone held, changes no v1
// object, and leaves the zone text equal to a store the build created.
func upgradeCheck(t *testing.T, tables []string) {
	t.Helper()
	path := zoneWithout(t, tables)
	raw := zoneRawDB(t, path)
	zoneMustExec(t, raw, "INSERT INTO dag_plans VALUES ('plan-old','PRJ-A','task-a','2026-10-11T00:00:00Z')")
	for _, table := range tables {
		if got := zoneStringList(t, raw, "SELECT name FROM sqlite_master WHERE tbl_name = '"+table+"'"); len(got) != 0 {
			t.Fatalf("the old zone still holds %v", got)
		}
	}
	v1 := zoneV1Catalog(t, path)
	rows := zoneRows(t, raw, "dag_plans")
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}
	zoneOpenClose(t, path)
	opened := zoneObjects(t, path)
	for _, table := range tables {
		if _, ok := opened["table "+table]; !ok {
			t.Errorf("the open did not add %s", table)
		}
		if _, ok := opened["trigger "+table+"_no_update"]; !ok {
			t.Errorf("the open did not add the guards of %s", table)
		}
	}
	if after := zoneV1Catalog(t, path); !reflect.DeepEqual(after, v1) {
		t.Error("the upgrade changed a v1 object")
	}
	db := zoneRawDB(t, path)
	if got := zoneRows(t, db, "dag_plans"); !slices.Equal(got, rows) {
		t.Errorf("a row of the old zone changed over the upgrade:\n%q\n%q", got, rows)
	}
	if missing := zoneMissingGuards(t, path); len(missing) != 0 {
		t.Errorf("guards missing after the open: %v", missing)
	}
	fresh := zonePreDAGStore(t)
	zoneOpenClose(t, fresh)
	if !reflect.DeepEqual(opened, zoneObjects(t, fresh)) {
		t.Error("the zone of an upgraded store and of a store the build created differ")
	}
	// a second open changes nothing
	zoneOpenClose(t, path)
	if again := zoneObjects(t, path); !reflect.DeepEqual(again, opened) {
		t.Error("a second open changed the zone")
	}
}

func TestFlowZoneUpgradeFromOldZone(t *testing.T) {
	t.Parallel()
	upgradeCheck(t, flowTables)
}

func TestAgentZoneUpgradeFromOldZone(t *testing.T) {
	t.Parallel()
	upgradeCheck(t, agentTables)
}

// The shipped snapshot (testdata/dag_zone_shipped.json) holds the text of every object of the new tables as it shipped, and the objects of
// the tables before them unchanged: the swap gate compares this text, so a statement of the zone is appended and never edited.
func TestZoneShippedSnapshot(t *testing.T) {
	t.Parallel()
	path := zonePreDAGStore(t)
	zoneOpenClose(t, path)
	live := zoneObjects(t, path)
	shipped := zoneShipped(t)
	var want []string
	for _, table := range slices.Concat(flowTables, agentTables, acceptTables, targetTables) {
		want = append(want, "table "+table, "trigger "+table+"_no_update", "trigger "+table+"_no_delete")
		for _, g := range zoneKeyGuardsOf(table) {
			want = append(want, "trigger "+g.name())
		}
		want = append(want, "trigger "+table+"_rowid_guard", "trigger "+table+"_rowid_minus1_mark_guard", "trigger "+table+"_rowid_minus1_guard")
	}
	for _, g := range zoneNullKeyGuards {
		if slices.Contains(flowTables, g.table) || slices.Contains(agentTables, g.table) || slices.Contains(acceptTables, g.table) || slices.Contains(targetTables, g.table) {
			want = append(want, "trigger "+g.insertTrigger())
		}
	}
	want = append(want, "index dag_agent_executions_node", "index dag_agent_executions_root", "index dag_agent_executions_successor", "trigger dag_agent_executions_chain_check",
		"index dag_agent_acceptances_node", "index dag_agent_acceptances_root", "index dag_agent_acceptances_successor", "trigger dag_agent_acceptances_execution_check", "trigger dag_agent_acceptances_chain_check",
		"index dag_agent_integration_stages_batch", "index dag_agent_integration_stages_acceptance", "trigger dag_agent_integration_stages_acceptance_check")
	for _, key := range want {
		text, ok := shipped[key]
		if !ok {
			t.Errorf("%s is not in the shipped snapshot", key)
			continue
		}
		if normalizeSQL(text) != live[key] {
			t.Errorf("%s: the snapshot and the zone differ:\n%s\n%s", key, normalizeSQL(text), live[key])
		}
	}
	var extra []string
	for key := range live {
		if _, ok := shipped[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	if len(extra) != 0 {
		t.Errorf("zone objects missing from the snapshot: %v", extra)
	}
	// the snapshot is a snapshot of the zone and of nothing else
	data, err := os.ReadFile(filepath.Join("testdata", "dag_zone_shipped.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Objects map[string]string `json:"objects"`
	}
	if err := json.Unmarshal(data, &doc); err != nil || len(doc.Objects) != len(live) {
		t.Errorf("the snapshot holds %d objects (%v), the zone %d", len(doc.Objects), err, len(live))
	}
}

// An execution row is never edited or removed, and its chain cannot branch: one row per node has no predecessor, a row is the predecessor of at
// most one row, and a row that is not an initial one names its predecessor.
func TestAgentExecutionAppendOnly(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	agentSeed(t, db)
	zoneMustExec(t, db, agentExecRow("'e1'", "'n1'", "'initial'", "NULL"), agentExecRow("'e2'", "'n1'", "'correction'", "'e1'"), agentCloseRow("'execution'", "'e2'", "'cancelled'"))
	for _, table := range agentTables {
		before := zoneRows(t, db, table)
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
	// the chain cannot branch or start twice
	zoneRefuses(t, db, "a second root of one node", agentExecRow("'e3'", "'n1'", "'initial'", "NULL"))
	zoneRefuses(t, db, "a second successor of one row", agentExecRow("'e4'", "'n1'", "'tier_step'", "'e1'"))
	zoneMustExec(t, db, agentExecRow("'e5'", "'n2'", "'initial'", "NULL")) // another node has its own root
	zoneMustExec(t, db, agentExecRow("'e6'", "'n1'", "'tier_step'", "'e2'"))
	zoneRefuses(t, db, "a correction with no predecessor", agentExecRow("'e7'", "'n3'", "'correction'", "NULL"))
	zoneRefuses(t, db, "a row that supersedes itself", agentExecRow("'e8'", "'n3'", "'initial'", "'e8'"))
	zoneRefuses(t, db, "a predecessor that does not exist", agentExecRow("'e9'", "'n3'", "'tier_step'", "'e-none'"))
	zoneRefuses(t, db, "a kind outside the vocabulary", agentExecRow("'e10'", "'n3'", "'redefinition'", "NULL"))
	zoneRefuses(t, db, "a manifest that is not stored", strings.Replace(agentExecRow("'e11'", "'n3'", "'initial'", "NULL"), zoneDigest, strings.Repeat("c", 64), 1))
	zoneRefuses(t, db, "a plan that does not exist", strings.Replace(agentExecRow("'e12'", "'n3'", "'initial'", "NULL"), "'plan-1'", "'plan-none'", 1))
	zoneRefuses(t, db, "an attempt of zero", agentExecRowAt("'e13'", "'plan-1'", "'n3'", "0", "'initial'", "NULL"))
	// a closure ends a subject once
	zoneRefuses(t, db, "a second closure of one execution", agentCloseRow("'execution'", "'e2'", "'escalated'"))
	zoneRefuses(t, db, "a closure state outside the vocabulary", agentCloseRow("'execution'", "'e1'", "'abandoned'"))
	zoneRefuses(t, db, "a closure subject outside the vocabulary", agentCloseRow("'node'", "'e1'", "'cancelled'"))
	zoneMustExec(t, db, agentCloseRow("'acceptance'", "'e2'", "'revoked'")) // the same id as an acceptance is another subject
}

// A chain stays on one node of one plan and runs forward: a row that supersedes a row of another node or another plan, or one that is not a later attempt, is refused, and what it would have followed stays
// the node's current last row (an INSERT whose foreign key only checked that the execution exists let another node's row end A's chain).
func TestAgentExecutionChainStaysOnOneNode(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	agentSeed(t, db)
	zoneMustExec(t, db, "INSERT INTO dag_plans VALUES ('plan-2','PRJ-A','task-a','2026-10-11T00:00:00Z')",
		agentExecRow("'a1'", "'nA'", "'initial'", "NULL"), agentExecRow("'i1'", "'nI'", "'initial'", "NULL"),
		agentExecRowAt("'p2'", "'plan-2'", "'nA'", "1", "'initial'", "NULL"))
	zoneMustExec(t, db, agentExecRowAt("'a2'", "'plan-1'", "'nA'", "3", "'correction'", "'a1'")) // a later attempt may skip numbers
	before := zoneRows(t, db, "dag_agent_executions")
	zoneRefuses(t, db, "a row of another node that supersedes A's", agentExecRowAt("'x-node'", "'plan-1'", "'nI'", "4", "'tier_step'", "'a2'"))
	zoneRefuses(t, db, "a row of another plan that supersedes A's", agentExecRowAt("'x-plan'", "'plan-2'", "'nA'", "4", "'tier_step'", "'a2'"))
	zoneRefuses(t, db, "a row of the same attempt", agentExecRowAt("'x-same'", "'plan-1'", "'nA'", "3", "'tier_step'", "'a2'"))
	zoneRefuses(t, db, "a row of an earlier attempt", agentExecRowAt("'x-back'", "'plan-1'", "'nA'", "2", "'tier_step'", "'a2'"))
	if after := zoneRows(t, db, "dag_agent_executions"); !slices.Equal(before, after) {
		t.Fatalf("a refused row changed the table:\n%v\n%v", before, after)
	}
	// the row that continues A's chain is still accepted, and it is the one that follows a2
	zoneMustExec(t, db, agentExecRowAt("'a3'", "'plan-1'", "'nA'", "4", "'tier_step'", "'a2'"))
	var successor string
	if err := db.QueryRow("SELECT execution_id FROM dag_agent_executions WHERE supersedes_execution_id = 'a2'").Scan(&successor); err != nil || successor != "a3" {
		t.Fatalf("the successor of a2 = %q (%v)", successor, err)
	}
}

// The key columns of the agent tables refuse NULL (CRW-835) and a REPLACE of a stored key or rowid (CRW-974), for each of the three keys of the
// execution table and for the closure's.
func TestAgentZoneNullAndNoReplaceGuards(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	agentSeed(t, db)
	for _, c := range []struct{ table, statement, want string }{
		{"dag_agent_executions", agentExecRow("NULL", "'n1'", "'initial'", "NULL"), "dag_agent_executions.execution_id is NULL: a row is addressed by a non-empty id"},
		{"dag_agent_closures", agentCloseRow("NULL", "'e1'", "'cancelled'"), "dag_agent_closures.subject_id is NULL: a row is addressed by a non-empty id"},
		{"dag_agent_closures", agentCloseRow("'execution'", "NULL", "'cancelled'"), "dag_agent_closures.subject_id is NULL: a row is addressed by a non-empty id"},
	} {
		if err := zoneExec(t, db, c.statement); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.statement, err, c.want)
		}
	}
	zoneMustExec(t, db, agentExecRow("'e1'", "'n1'", "'initial'", "NULL"), agentExecRow("'e2'", "'n1'", "'correction'", "'e1'"), agentCloseRow("'execution'", "'e2'", "'cancelled'"))
	replace := func(statement string) string {
		return strings.Replace(statement, "INSERT INTO", "INSERT OR REPLACE INTO", 1)
	}
	cases := []struct{ table, statement, want string }{
		{"dag_agent_executions", replace(agentExecRow("'e1'", "'n9'", "'initial'", "NULL")), "UNIQUE constraint failed: dag_agent_executions.execution_id"},
		{"dag_agent_executions", replace(agentExecRow("'e3'", "'n1'", "'initial'", "NULL")), "UNIQUE constraint failed: dag_agent_executions.plan_id, dag_agent_executions.node_id"},
		{"dag_agent_executions", replace(agentExecRow("'e4'", "'n1'", "'tier_step'", "'e1'")), "UNIQUE constraint failed: dag_agent_executions.supersedes_execution_id"},
		{"dag_agent_closures", replace(agentCloseRow("'execution'", "'e2'", "'revoked'")), "UNIQUE constraint failed: dag_agent_closures.subject_kind, dag_agent_closures.subject_id"},
	}
	for _, c := range cases {
		before := zoneRows(t, db, c.table)
		if err := zoneExec(t, db, c.statement); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.statement, err, c.want)
		}
		if after := zoneRows(t, db, c.table); !slices.Equal(before, after) {
			t.Errorf("%s changed over a refused REPLACE", c.table)
		}
	}
	for _, table := range agentTables {
		var rowid int64
		if err := db.QueryRow("SELECT rowid FROM " + table + " ORDER BY rowid LIMIT 1").Scan(&rowid); err != nil {
			t.Fatal(err)
		}
		statement := agentExecRow("'e-free'", "'n-free'", "'initial'", "NULL")
		if table == "dag_agent_closures" {
			statement = agentCloseRow("'execution'", "'e-free'", "'cancelled'")
		}
		statement = strings.Replace(statement, "INSERT INTO "+table+" (", fmt.Sprintf("INSERT OR REPLACE INTO %s (rowid, ", table), 1)
		statement = strings.Replace(statement, "VALUES (", fmt.Sprintf("VALUES (%d, ", rowid), 1)
		if err := zoneExec(t, db, statement); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+table+".rowid") {
			t.Errorf("%s: REPLACE of the stored rowid: %v", table, err)
		}
	}
}
