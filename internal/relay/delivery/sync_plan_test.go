package delivery

import (
	"testing"
)

// CRW-1216 (R08, the R11 stop): a verdict on a relationship the DAG plan runs is projected to Linear by the plan's summary outbox (dagsched/outbox.go, one row per issue), so the per-verdict
// sync_outbox enqueue stops for it. A relationship that no plan runs keeps the enqueue as it always had. The tests use temporary stores only.

// planBound binds the relationship to a plan node the way a release does (the row of dag_node_executions the plan keeps for every relationship it started).
func planBound(t *testing.T, v *vcu, plan, node string) {
	t.Helper()
	rcExec(t, v.fixture, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?,?,?,?,?,?,?)",
		plan, node, v.rid, 1, "manifest-"+node, "initial", "request-"+node)
}

func syncedVerdict(t *testing.T, bind bool) (*vcu, string) {
	t.Helper()
	return syncedVerdictNode(t, bind, 0)
}

// syncedVerdictNode is syncedVerdict whose bound node is retired at the plan revision retireAt (0: the node is live).
func syncedVerdictNode(t *testing.T, bind bool, retireAt int) (*vcu, string) {
	t.Helper()
	v := newVCU(t, "")
	e := v.acknowledged("")
	rcExec(t, v.fixture, "INSERT INTO sync_targets (relationship_id, target, target_ref, recorded_at) VALUES (?,?,?,?)", v.rid, "coordination_document", "DOC-1", v.clock.ISO())
	v.ack.Sync = VerdictSync(v.store, v.clock)
	if bind {
		planBound(t, v, "plan-1", "node-1")
		planNode(t, v, "plan-1", "node-1", retireAt)
	}
	return v, e
}

func syncRows(t *testing.T, v *vcu) int {
	t.Helper()
	return int(v.one("SELECT COUNT(*) AS n FROM sync_outbox").I("n"))
}

func TestPlanVerdictSkipsSyncOutbox(t *testing.T) {
	t.Parallel()
	v, e := syncedVerdict(t, true)
	if got := v.verdict(e, "needs_changes", "v1", []any{finding("c1", "needs_changes", "the endpoint answers the wrong shape")}, nil, nil); got["ok"] == nil {
		t.Fatalf("the verdict was refused: %v", got)
	}
	if n := syncRows(t, v); n != 0 {
		t.Fatalf("a verdict on a relationship of a plan wrote %d sync_outbox rows: the plan's summary outbox carries it", n)
	}
	// the verdict itself is recorded: only the projection moved
	if got := v.one("SELECT verdict FROM verdicts WHERE event_id = ?", e); got.S("verdict") != "needs_changes" {
		t.Fatalf("the ruling was not recorded: %v", got)
	}
	if n := v.one("SELECT COUNT(*) AS n FROM journal WHERE kind = 'sync_enqueued'").I("n"); n != 0 {
		t.Fatalf("a sync_enqueued journal entry was written for a verdict that enqueued nothing: %d", n)
	}
}

func TestNonPlanVerdictKeepsSyncOutbox(t *testing.T) {
	t.Parallel()
	v, e := syncedVerdict(t, false)
	if got := v.verdict(e, "needs_changes", "v1", []any{finding("c1", "needs_changes", "the endpoint answers the wrong shape")}, nil, nil); got["ok"] == nil {
		t.Fatalf("the verdict was refused: %v", got)
	}
	if n := syncRows(t, v); n != 1 {
		t.Fatalf("a verdict on a relationship no plan runs wrote %d sync_outbox rows, want 1", n)
	}
	row := v.one("SELECT relationship_id, subject_kind, verdict, state FROM sync_outbox")
	if row.S("relationship_id") != v.rid || row.S("subject_kind") != "verdict" || row.S("verdict") != "needs_changes" || row.S("state") != "pending" {
		t.Fatalf("the row is %v", row)
	}
}

// A plan that runs other relationships does not make this one a plan relationship: the binding is the relationship's own row.
func TestNonPlanVerdictKeepsSyncOutboxBesideAnotherPlansRelationship(t *testing.T) {
	t.Parallel()
	v, e := syncedVerdict(t, false)
	rcExec(t, v.fixture, "INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind) VALUES ('plan-x','node-x','some-other-relationship',1,'m','initial')")
	if got := v.verdict(e, "verified", "v1", nil, nil, nil); got["ok"] == nil {
		t.Fatalf("the verdict was refused: %v", got)
	}
	if n := syncRows(t, v); n != 1 {
		t.Fatalf("a relationship that no plan runs wrote %d sync_outbox rows, want 1", n)
	}
}

// planNode makes the node a live node of the plan (dag_nodes holds a live version of it), the way the plan's fold keeps it. retireAt > 0 retires that version at that plan revision.
func planNode(t *testing.T, v *vcu, plan, node string, retireAt int) {
	t.Helper()
	revisions := 1
	if retireAt > 1 {
		revisions = retireAt
	}
	rcExec(t, v.fixture, "INSERT OR IGNORE INTO dag_plans (plan_id, project_key, created_by_task_id, created_at) VALUES (?,?,?,?)", plan, "PRJ", "parent", v.clock.ISO())
	for rev := 1; rev <= revisions; rev++ {
		rcExec(t, v.fixture, "INSERT OR IGNORE INTO dag_plan_revisions (plan_id, revision_no, parent_revision_no, request_id, request_digest, change_json, state_digest, author_task_id, recorded_at) VALUES (?,?,?,?,?,?,?,?,?)",
			plan, rev, rev-1, "request-"+plan+"-"+string(rune('0'+rev)), "digest", "[]", "state", "parent", v.clock.ISO())
	}
	var retired any
	if retireAt > 0 {
		retired = retireAt
	}
	rcExec(t, v.fixture, "INSERT INTO dag_nodes (plan_id, node_id, introduced_rev, retired_rev, slice_digest, issue_key, node_kind, criteria_set_digest) VALUES (?,?,?,?,?,?,?,?)",
		plan, node, 1, retired, "slice", "ISS-1", "implementation", "criteria")
}

// A relationship bound to a node that is no longer live in the plan (retired or replaced) is not carried by any summary row, so its verdict keeps the sync_outbox row it always had (d2).
func TestRetiredPlanNodeVerdictKeepsSyncOutbox(t *testing.T) {
	t.Parallel()
	v, e := syncedVerdictNode(t, true, 2)
	if got := v.verdict(e, "needs_changes", "v1", []any{finding("c1", "needs_changes", "the endpoint answers the wrong shape")}, nil, nil); got["ok"] == nil {
		t.Fatalf("the verdict was refused: %v", got)
	}
	if n := syncRows(t, v); n != 1 {
		t.Fatalf("a verdict on a relationship of a retired node wrote %d sync_outbox rows, want 1: no summary row carries it", n)
	}
}
