package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// CRW-1213 (R05): the agent acceptance rows (dag_agent_acceptances) and the node-level integration stages of an agent acceptance (dag_agent_integration_stages), appended to the zone ledger after
// the agent execution tables. Both are append-only, refuse a NULL key (CRW-835) and a REPLACE of a stored key or rowid (CRW-974, the generic key-guard tests take their keys), and an old zone that lacks
// them is extended by the next write open with every row kept. These tests write the tables directly, as an operator's sqlite3 would, on a store the build opened (foreign keys on).

// agentAcceptRow is the INSERT of one acceptance. id, node, execution, head, criteria and supersedes are SQL literals; the manifest is the one the execution rows of flowAgentSeed carry.
// A head of ” is the acceptance of a node without a head: everything the head carries is ” too.
func agentAcceptRow(id, plan, node, manifest, execution, head, criteria, supersedes string) string {
	body := "'/repo','p-digest','{\"schema\":\"premerge-record/1\"}','e-head','v-digest','{\"schema\":\"verification-record/1\"}','tree'"
	base := "'base'"
	if head == "''" {
		body, base = "'','','','','','',''", "''"
	}
	return fmt.Sprintf("INSERT INTO dag_agent_acceptances (acceptance_id, plan_id, node_id, manifest_digest, execution_id, head, base, repository, premerge_digest, premerge_json, premerge_evaluated_head, verification_digest, verification_json, tree_sha,"+
		" criteria_set_digest, reviewer_call_id, reviewer_thread_id, reviewer_turn_id, reviewer_model, reviewer_effort, author_pairs_digest, accepted_by_task_id, coordinator_epoch, accepted_at, supersedes_acceptance_id)"+
		" VALUES (%s,%s,%s,%s,%s,%s,%s,%s,%s,'rev-call','rev-thread','rev-turn','gpt-fake-sol','high','authors','task-orchestrator',1,'2026-10-11T00:00:07Z',%s)",
		id, plan, node, manifest, execution, head, base, body, criteria, supersedes)
}

// acceptOf is agentAcceptRow for plan-1 under the first manifest of agentSeed.
func acceptOf(id, node, execution, head, criteria, supersedes string) string {
	return agentAcceptRow(id, "'plan-1'", node, "'"+zoneDigest+"'", execution, head, criteria, supersedes)
}

func agentStageRow(id, batch, node, acceptance, execution, stage, head string) string {
	return fmt.Sprintf("INSERT INTO dag_agent_integration_stages (stage_id, batch_id, plan_id, node_id, acceptance_id, execution_id, stage, head, detail, recorded_by, recorded_at)"+
		" VALUES (%s,%s,'plan-1',%s,%s,%s,%s,%s,'detail','task-orchestrator','2026-10-11T00:00:08Z')", id, batch, node, acceptance, execution, stage, head)
}

// A row of the acceptance chain or of the stages is never edited or removed, and the chain cannot branch: one row per node has no predecessor, a row is the predecessor of at most one row, the key
// (execution, head, criteria set) belongs to one row, and a row stays on the node, plan, manifest and execution it names.
func TestAgentAcceptanceAppendOnlyGuards(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	agentSeed(t, db)
	zoneMustExec(t, db, agentExecRow("'e1'", "'n1'", "'initial'", "NULL"), agentExecRow("'e2'", "'n1'", "'correction'", "'e1'"), agentExecRow("'e5'", "'n2'", "'initial'", "NULL"),
		acceptOf("'a1'", "'n1'", "'e1'", "'h1'", "'c1'", "NULL"),
		acceptOf("'a2'", "'n1'", "'e2'", "'h2'", "'c1'", "'a1'"),
		agentStageRow("'s1'", "'b1'", "'n1'", "'a2'", "'e2'", "'intent'", "'h2'"),
		agentStageRow("'s2'", "'b1'", "'n1'", "'a2'", "'e2'", "'marked'", "'h2'"))
	for _, table := range acceptTables {
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
	// the chain cannot branch or start twice
	zoneRefuses(t, db, "a second root of one node", acceptOf("'a3'", "'n1'", "'e2'", "'h3'", "'c1'", "NULL"))
	zoneRefuses(t, db, "a second successor of one row", acceptOf("'a4'", "'n1'", "'e2'", "'h4'", "'c1'", "'a1'"))
	zoneMustExec(t, db, acceptOf("'a5'", "'n2'", "'e5'", "'h5'", "'c1'", "NULL")) // another node has its own root
	// the key (execution, head, criteria set): the same acceptance again is refused as another row, whichever id it carries; another criteria set of the same execution and head is another row
	zoneRefuses(t, db, "the same execution, head and criteria set under another id", acceptOf("'a6'", "'n1'", "'e2'", "'h2'", "'c1'", "'a2'"))
	zoneMustExec(t, db, acceptOf("'a7'", "'n1'", "'e2'", "'h2'", "'c2'", "'a2'")) // a criteria-only re-review (R07) supersedes the row it re-reviews
	// a row stays on the node, the plan and the manifest of its execution, and its chain on one node
	zoneRefuses(t, db, "an acceptance of another node's execution", acceptOf("'x1'", "'n2'", "'e1'", "'hx'", "'c1'", "'a5'"))
	zoneRefuses(t, db, "an acceptance that supersedes another node's", acceptOf("'x2'", "'n2'", "'e5'", "'hy'", "'c2'", "'a7'"))
	zoneRefuses(t, db, "an acceptance under another manifest than its execution's", agentAcceptRow("'x3'", "'plan-1'", "'n2'", "'"+strings.Repeat("b", 64)+"'", "'e5'", "'hz'", "'c3'", "'a5'"))
	zoneRefuses(t, db, "an acceptance of an execution that does not exist", acceptOf("'x4'", "'n9'", "'e-none'", "'h1'", "'c1'", "NULL"))
	zoneRefuses(t, db, "a predecessor that does not exist", acceptOf("'x5'", "'n9'", "'e5'", "'h1'", "'c1'", "'a-none'"))
	zoneRefuses(t, db, "a plan that does not exist", agentAcceptRow("'x6'", "'plan-none'", "'n2'", "'"+zoneDigest+"'", "'e5'", "'h9'", "'c1'", "'a5'"))
	zoneRefuses(t, db, "a row that supersedes itself", acceptOf("'x7'", "'n9'", "'e5'", "'h9'", "'c1'", "'x7'"))
	zoneRefuses(t, db, "an empty criteria set", acceptOf("'x8'", "'n2'", "'e5'", "'h8'", "''", "'a5'"))
	// a head and what it carries go together
	zoneRefuses(t, db, "a head without its verification record", strings.Replace(acceptOf("'x9'", "'n2'", "'e5'", "'h9'", "'c9'", "'a5'"), "'{\"schema\":\"verification-record/1\"}'", "''", 1))
	zoneRefuses(t, db, "a record without a head", strings.Replace(acceptOf("'x10'", "'n2'", "'e5'", "''", "'c10'", "'a5'"), "'','','','','','',''", "'','','','','','','tree'", 1))
	// a node without a head has one acceptance per execution and criteria set
	zoneMustExec(t, db, agentExecRow("'e8'", "'n8'", "'initial'", "NULL"), acceptOf("'n8a'", "'n8'", "'e8'", "''", "'c1'", "NULL"))
	zoneRefuses(t, db, "the headless acceptance again", acceptOf("'n8b'", "'n8'", "'e8'", "''", "'c1'", "'n8a'"))
	// a closure of kind acceptance ends an acceptance without editing it
	zoneMustExec(t, db, agentCloseRow("'acceptance'", "'a7'", "'revoked'"))
	zoneRefuses(t, db, "a second closure of the acceptance", agentCloseRow("'acceptance'", "'a7'", "'superseded'"))
	if rows := zoneRows(t, db, "dag_agent_acceptances"); len(rows) != 5 {
		t.Errorf("dag_agent_acceptances holds %d rows after the refusals, want the 5 that were accepted: %q", len(rows), rows)
	}

	// the stages: the vocabulary is the node-level one (the batch-level rows stay in dag_integration_stages), and a stage names the acceptance of its own plan, node, execution and head
	zoneRefuses(t, db, "a batch-level stage", agentStageRow("'s3'", "'b1'", "'n1'", "'a2'", "'e2'", "'ref_moved'", "'h2'"))
	zoneRefuses(t, db, "a stage of an unknown stage", agentStageRow("'s4'", "'b1'", "'n1'", "'a2'", "'e2'", "'merged'", "'h2'"))
	zoneRefuses(t, db, "a stage with no node", agentStageRow("'s5'", "'b1'", "''", "'a2'", "'e2'", "'intent'", "'h2'"))
	zoneRefuses(t, db, "a stage with no head", agentStageRow("'s6'", "'b1'", "'n1'", "'a2'", "'e2'", "'intent'", "''"))
	zoneRefuses(t, db, "a stage of an acceptance that does not exist", agentStageRow("'s7'", "'b1'", "'n1'", "'a-none'", "'e2'", "'intent'", "'h2'"))
	zoneRefuses(t, db, "a stage of another node's acceptance", agentStageRow("'s8'", "'b1'", "'n2'", "'a2'", "'e2'", "'marked'", "'h2'"))
	zoneRefuses(t, db, "a stage under another execution than the acceptance's", agentStageRow("'s9'", "'b1'", "'n1'", "'a2'", "'e1'", "'marked'", "'h2'"))
	zoneRefuses(t, db, "a stage on another head than the acceptance's", agentStageRow("'s10'", "'b1'", "'n1'", "'a2'", "'e2'", "'marked'", "'h1'"))
	zoneRefuses(t, db, "a repeated stage id", agentStageRow("'s1'", "'b2'", "'n1'", "'a2'", "'e2'", "'intent'", "'h2'"))
	zoneMustExec(t, db, agentStageRow("'s11'", "'b1'", "'n1'", "'a2'", "'e2'", "'mark_pending'", "'h2'"))
	if rows := zoneRows(t, db, "dag_agent_integration_stages"); len(rows) != 3 {
		t.Errorf("dag_agent_integration_stages holds %d rows after the refusals, want 3: %q", len(rows), rows)
	}
}

// The key columns of both tables refuse NULL (CRW-835), and a REPLACE of a stored key or rowid is refused with the text of a plain INSERT (CRW-974): the rows stay as they were.
func TestAgentAcceptanceNullAndNoReplaceGuards(t *testing.T) {
	t.Parallel()
	db := openedZone(t)
	agentSeed(t, db)
	zoneMustExec(t, db, agentExecRow("'e1'", "'n1'", "'initial'", "NULL"))
	for _, c := range []struct{ table, statement, want string }{
		{"dag_agent_acceptances", acceptOf("NULL", "'n1'", "'e1'", "'h1'", "'c1'", "NULL"), "dag_agent_acceptances.acceptance_id is NULL: a row is addressed by a non-empty id"},
		{"dag_agent_integration_stages", agentStageRow("NULL", "'b1'", "'n1'", "'a1'", "'e1'", "'intent'", "'h1'"), "dag_agent_integration_stages.stage_id is NULL: a row is addressed by a non-empty id"},
	} {
		if err := zoneExec(t, db, c.statement); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v; want %q", c.statement, err, c.want)
		}
	}
	zoneMustExec(t, db, acceptOf("'a1'", "'n1'", "'e1'", "'h1'", "'c1'", "NULL"), agentStageRow("'s1'", "'b1'", "'n1'", "'a1'", "'e1'", "'intent'", "'h1'"))
	for _, c := range []struct{ table, key, statement, free string }{
		{"dag_agent_acceptances", "dag_agent_acceptances.acceptance_id", acceptOf("'a1'", "'n1'", "'e1'", "'h-pk'", "'c1'", "'a1'"), acceptOf("'a-free'", "'n1'", "'e1'", "'h-free'", "'c1'", "'a1'")},
		{"dag_agent_acceptances", "dag_agent_acceptances.execution_id, dag_agent_acceptances.head, dag_agent_acceptances.criteria_set_digest", acceptOf("'a2'", "'n1'", "'e1'", "'h1'", "'c1'", "'a1'"), ""},
		{"dag_agent_integration_stages", "dag_agent_integration_stages.stage_id", agentStageRow("'s1'", "'b2'", "'n1'", "'a1'", "'e1'", "'intent'", "'h1'"), agentStageRow("'s-free'", "'b2'", "'n1'", "'a1'", "'e1'", "'intent'", "'h1'")},
	} {
		before := zoneRows(t, db, c.table)
		for _, verb := range []string{"INSERT OR REPLACE INTO", "INSERT INTO", "REPLACE INTO"} {
			statement := strings.Replace(c.statement, "INSERT INTO", verb, 1)
			if err := zoneExec(t, db, statement); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+c.key) {
				t.Errorf("%s: %s: %v; want the refusal of %s", c.table, verb, err, c.key)
			}
		}
		if c.free != "" {
			// a row with a free key that names the rowid of the stored row replaces it on an unguarded table
			var rowid int64
			if err := db.QueryRow("SELECT rowid FROM " + c.table).Scan(&rowid); err != nil {
				t.Fatal(err)
			}
			replace := strings.Replace(c.free, "INSERT INTO "+c.table+" (", fmt.Sprintf("INSERT OR REPLACE INTO %s (rowid, ", c.table), 1)
			replace = strings.Replace(replace, "VALUES (", fmt.Sprintf("VALUES (%d, ", rowid), 1)
			if err := zoneExec(t, db, replace); err == nil || !strings.Contains(err.Error(), "UNIQUE constraint failed: "+c.table+".rowid") {
				t.Errorf("%s: REPLACE of the stored rowid: %v", c.table, err)
			}
		}
		if after := zoneRows(t, db, c.table); !slices.Equal(before, after) {
			t.Errorf("%s changed over refused REPLACEs:\n%q\n%q", c.table, before, after)
		}
	}
}

// A zone that ends before the acceptance tables is extended by the open that finds them missing, with every row it held kept and the text of a store the build created.
func TestAgentAcceptanceZoneUpgradeFromOldZone(t *testing.T) {
	t.Parallel()
	upgradeCheck(t, acceptTables)
}
