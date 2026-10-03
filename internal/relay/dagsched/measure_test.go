package dagsched

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-411, criterion c3: the measurements of a plan are read from the store by one query, and a value with no data reads as absent with its reason, never as zero.

func measured(t *testing.T, f *fixture, plan string) map[string]any {
	t.Helper()
	got, err := f.sched.Measurements(context.Background(), plan)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(pyjson.Dumps(got.Object(), pyjson.Options{Compact: true})), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func metric(t *testing.T, m map[string]any, keys ...string) map[string]any {
	t.Helper()
	cur := m
	for _, k := range keys {
		next, ok := cur[k].(map[string]any)
		if !ok {
			t.Fatalf("%v has no object %s: %v", keys, k, cur)
		}
		cur = next
	}
	return cur
}

func wantAbsent(t *testing.T, m map[string]any, reason string, keys ...string) {
	t.Helper()
	got := metric(t, m, keys...)
	if got["absent"] != reason {
		t.Fatalf("%v = %v, want it absent (%s)", keys, got, reason)
	}
	for k := range got {
		if k != "samples" && k != "absent" {
			t.Fatalf("%v is absent and still prints %s: %v", keys, k, got)
		}
	}
}

func wantPresent(t *testing.T, m map[string]any, samples float64, keys ...string) map[string]any {
	t.Helper()
	got := metric(t, m, keys...)
	if got["absent"] != nil || got["samples"] != samples {
		t.Fatalf("%v = %v, want %v samples and no absent reason", keys, got, samples)
	}
	return got
}

// An empty plan has no data for anything: every measure is absent with its reason, and none reads zero.
func TestAMeasureWithNoDataIsAbsentNotZero(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("m", 0, "m-r1", addNode("a", dag.NodeImplementation), addNode("b", dag.NodeImplementation))
	got := measured(t, f, "m")
	wantAbsent(t, got, AbsentNoRecordedPass, "parallelism")
	wantAbsent(t, got, AbsentNoObservations, "conflicts_by_grade")
	wantAbsent(t, got, AbsentNoLandings, "conflict_handling")
	wantAbsent(t, got, AbsentNoLandings, "base_refresh", "stale_base_judgements")
	wantAbsent(t, got, AbsentNoLandings, "base_refresh", "returns_to_child")
	wantAbsent(t, got, AbsentNotRecorded, "base_refresh", "round_trips")
	wantAbsent(t, got, AbsentNoLandings, "post_merge")
	wantAbsent(t, got, AbsentNoneRecorded, "duplicated_or_discarded")
	// the plan's own record has a real zero: no node was cancelled after a child was created
	if nodes := wantPresent(t, got, 2, "cancelled_after_release"); nodes["nodes"] != float64(0) {
		t.Fatalf("cancelled after release = %v, want 0 of 2 nodes", nodes)
	}
	if got["landed_nodes"] != float64(0) || len(got["landings"].([]any)) != 0 {
		t.Fatalf("landings = %v", got["landings"])
	}
}

// Landings that nothing measured read absent, not zero: no sweep found a conflict, no merge judgement exists, the parent recorded nothing.
func TestLandingsWithNothingMeasuredAreAbsentNotZero(t *testing.T) {
	w := newPolicyWorld(t, "l1", "l2")
	w.land("l1", 1, 0)
	w.land("l2", 2, 0)
	got := measured(t, w.f, "p")
	wantAbsent(t, got, AbsentNoObservations, "conflict_handling")
	wantAbsent(t, got, AbsentNoObservations, "base_refresh", "stale_base_judgements")
	// every landed node has an execution row, and none was sent back: a count over rows that exist, so it is a real zero
	if returns := wantPresent(t, got, 2, "base_refresh", "returns_to_child"); returns["total"] != float64(0) || returns["max"] != float64(0) {
		t.Fatalf("returns to the child = %v", returns)
	}
	if samples := metric(t, got, "post_merge")["samples"]; samples != float64(2) {
		t.Fatalf("post-merge samples = %v, want the 2 landings", samples)
	}
	wantAbsent2 := metric(t, got, "post_merge")
	if wantAbsent2["absent"] != AbsentNoneRecorded {
		t.Fatalf("post merge = %v, want it absent: the parent recorded nothing", wantAbsent2)
	}
	for _, l := range got["landings"].([]any) {
		row := l.(map[string]any)
		if row["conflict_handling_seconds"] != nil || row["stale_base_judgements"] != nil {
			t.Fatalf("a landing nothing measured prints a number: %v", row)
		}
	}
}

func (w *policyWorld) judge(node, outcome string, seq int, at time.Time) {
	w.t.Helper()
	var acceptance string
	if err := w.f.s.DB.QueryRowContext(context.Background(), "SELECT acceptance_id FROM dag_acceptances WHERE plan_id = 'p' AND node_id = ?", node).Scan(&acceptance); err != nil {
		w.t.Fatal(err)
	}
	w.f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
		" VALUES (?,?,?,?,?,?,?,?,?,1,?,?,?)", fmt.Sprintf("chk-%s-%d", node, seq), acceptance, seq, head1, head1, "tip", "digest", "{}", "[]", outcome, outcome, stamp(at))
}

func TestMeasurementsReadTheStore(t *testing.T) {
	w := newPolicyWorld(t, "l1", "l2", "l3")
	f := w.f

	// two recorded passes: the second with two slots held
	first, _ := w.pass()
	f.holdSlots(2)
	second, _ := w.pass()

	// l1 settled a conflict for 900 seconds, was found behind twice and sent back once; l2 had a clean merge judgement; l3 was never judged
	w.land("l1", 5, 900)
	w.judge("l1", "stale_base", 1, policyAt(1))
	w.judge("l1", "checks_pending", 2, policyAt(2))
	w.judge("l1", "stale_base", 3, policyAt(3))
	w.judge("l1", "eligible", 4, policyAt(4))
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES ('p', 'l1', 'rel-p-l1', 2, ?, 'correction', NULL)", dig("correction"))
	w.land("l2", 6, 0)
	w.judge("l2", "eligible", 1, policyAt(1))
	w.land("l3", 7, 0)

	w.result("l1", ResultDevRed)
	w.result("l2", ResultDevGreen)
	for _, in := range []struct{ node, kind, evidence string }{{"cand", ResultDuplicate, "same function as l1"}, {"hold", ResultDiscarded, "closed unmerged"}, {"hold", ResultDiscarded, "closed again"}} {
		if _, err := f.sched.RecordLandingResult(context.Background(), "p", in.node, "parent", ResultInput{Kind: in.kind, Evidence: in.evidence}); err != nil {
			t.Fatal(err)
		}
	}

	// a conflict between the holder and the candidate on a.go (declared by both) and b.go (declared by neither), measured by a sweep, and one of l1's head against a tip
	f.exec("INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at) VALUES ('dco-1', 'p', 'cand', 'hold', 'owner/repo', 'x', 'y', 'z', 2, 'git', 'parent', ?)", stamp(policyAt(0)))
	f.exec("INSERT INTO dag_conflict_observation_files (observation_id, repository, path) VALUES ('dco-1', 'owner/repo', 'a.go'), ('dco-1', 'owner/repo', 'b.go')")
	f.exec("INSERT INTO dag_tip_conflict_observations (observation_id, plan_id, node_id, repository, head, head_source, tip_ref, tip_sha, base_sha, conflict_count, method, observed_by, observed_at) VALUES ('dto-9', 'p', 'l1', 'owner/repo', 'x', 'acceptance', 'dev', 'tip', 'z', 1, 'git', 'parent', ?)", stamp(policyAt(0)))
	f.exec("INSERT INTO dag_tip_conflict_observation_files (observation_id, repository, path) VALUES ('dto-9', 'owner/repo', 'z.go')")
	seq := f.count("SELECT COALESCE(MAX(sweep_seq), 0) + 1 FROM dag_conflict_sweeps WHERE plan_id = 'p'")
	f.exec("INSERT INTO dag_conflict_sweeps (plan_id, sweep_seq, trigger_kind, trigger_node, trigger_ref, repository, observed_by, observed_at) VALUES ('p', ?, 'manual', '', '', 'owner/repo', 'parent', ?)", seq, stamp(policyAt(0)))
	f.exec("INSERT INTO dag_conflict_sweep_members (plan_id, sweep_seq, member_seq, kind, left_node_id, right_node_id, left_head, right_head, status, observation_id, conflicts) VALUES ('p', ?, 1, 'pair', 'cand', 'hold', 'x', 'y', 'observed', 'dco-1', 2)", seq)

	got := measured(t, f, "p")
	if got["schema"] != "dag-measurements/1" || got["landed_nodes"] != float64(3) {
		t.Fatalf("header = %v", got)
	}

	par := wantPresent(t, got, 2, "parallelism")
	held := metric(t, par, "held_slots")
	if held["latest"] != float64(second.Pass.Held) || held["max"] != float64(second.Pass.Held) || held["mean"] != round2(float64(first.Pass.Held+second.Pass.Held)/2) || second.Pass.Held != 2 {
		t.Fatalf("held slots = %v (passes held %d and %d)", held, first.Pass.Held, second.Pass.Held)
	}
	if free := metric(t, par, "free_slots"); free["latest"] != float64(second.Pass.FreeSlots) || free["min"] != float64(second.Pass.FreeSlots) {
		t.Fatalf("free slots = %v", free)
	}
	if lim := metric(t, par, "limited_by"); lim["edit_overlap"] != float64(0) {
		t.Fatalf("limited by = %v", lim)
	}

	conflicts := wantPresent(t, got, 3, "conflicts_by_grade")
	local := metric(t, conflicts, "by_grade", "local")
	if local["prs"] != float64(2) || local["files"] != float64(4) || local["files_per_pr"] != float64(2) {
		t.Fatalf("local conflicts = %v, want 2 pull requests and 4 files", local)
	}
	for _, grade := range []string{"mechanical", "exclusive"} {
		if g := metric(t, conflicts, "by_grade", grade); g["prs"] != float64(0) || g["files"] != float64(0) || g["files_per_pr"] != nil {
			t.Fatalf("%s conflicts = %v, want counts of zero over measured pull requests and no per-PR figure", grade, g)
		}
	}
	if tip := metric(t, conflicts, "against_tip"); tip["prs"] != float64(1) || tip["files"] != float64(1) {
		t.Fatalf("against the tip = %v", tip)
	}
	wantAbsent(t, conflicts, AbsentNotRecorded, "hunks")

	handling := wantPresent(t, got, 1, "conflict_handling")
	if handling["median_seconds"] != float64(900) || handling["mean_seconds"] != float64(900) || handling["max_seconds"] != float64(900) {
		t.Fatalf("handling = %v", handling)
	}

	stale := wantPresent(t, got, 2, "base_refresh", "stale_base_judgements")
	if stale["total"] != float64(2) || stale["max"] != float64(2) || stale["mean_per_landing"] != float64(1) {
		t.Fatalf("stale-base judgements = %v (l3 was never judged: it is not a zero)", stale)
	}
	returns := wantPresent(t, got, 3, "base_refresh", "returns_to_child")
	if returns["total"] != float64(1) || returns["max"] != float64(1) || returns["mean_per_landing"] != round2(1.0/3) {
		t.Fatalf("returns to the child = %v", returns)
	}
	wantAbsent(t, got, AbsentNotRecorded, "base_refresh", "round_trips")

	post := wantPresent(t, got, 3, "post_merge")
	if post["with_a_result"] != float64(2) || post["dev_green"] != float64(1) || post["dev_red"] != float64(1) || post["reverted"] != float64(0) {
		t.Fatalf("post merge = %v", post)
	}
	dup := wantPresent(t, got, 2, "duplicated_or_discarded")
	if dup["duplicate_nodes"] != float64(1) || dup["discarded_nodes"] != float64(1) {
		t.Fatalf("duplicated or discarded = %v: a node counts once per kind", dup)
	}

	rows := got["landings"].([]any)
	if len(rows) != 3 || rows[2].(map[string]any)["node_id"] != "l3" || rows[2].(map[string]any)["stale_base_judgements"] != nil || rows[0].(map[string]any)["stale_base_judgements"] != float64(2) {
		t.Fatalf("landing rows = %v", rows)
	}
}

// A store whose zone predates the tables that hold a measure reads that measure absent with the reason, and the rest as it is.
func TestMeasurementsOfAnOlderZone(t *testing.T) {
	w := newPolicyWorld(t, "l1")
	w.land("l1", 1, 0)
	for _, table := range []string{"dag_pass_release_policy", "dag_release_policy", "dag_landing_results", "dag_conflict_sweep_members"} {
		w.f.exec("DROP TABLE " + table)
	}
	got := measured(t, w.f, "p")
	if got["landed_nodes"] != float64(1) {
		t.Fatalf("landings = %v", got["landings"])
	}
	wantAbsent2 := metric(t, got, "post_merge")
	if wantAbsent2["absent"] != AbsentTableMissing || wantAbsent2["samples"] != float64(1) {
		t.Fatalf("post merge = %v, want it absent: the table is missing", wantAbsent2)
	}
	wantAbsent(t, got, AbsentTableMissing, "duplicated_or_discarded")
	wantAbsent(t, got, AbsentTableMissing, "conflicts_by_grade")
	wantAbsent(t, got, AbsentNoObservations, "conflict_handling")
	if _, _, err := w.f.sched.RecordPass(context.Background(), "p", "parent", ReadyOptions{}); err != nil {
		t.Fatalf("a pass on that store: %v", err)
	}
}

func TestCancelledAfterReleaseCountsNodesThatHadAChild(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	f.putPlan("c", 0, "c-r1", addNode("x", dag.NodeImplementation), addNode("y", dag.NodeImplementation), addNode("z", dag.NodeImplementation))
	f.startNode("c", "x")
	f.putPlan("c", 1, "c-r2", lifeOp("cancel_node", "x"), lifeOp("cancel_node", "y"))
	got := measured(t, f, "c")
	if cancelled := wantPresent(t, got, 3, "cancelled_after_release"); cancelled["nodes"] != float64(1) {
		t.Fatalf("cancelled after release = %v, want x only: y never had a child", cancelled)
	}
	wantAbsent(t, got, AbsentNoneRecorded, "duplicated_or_discarded")
}

// The observations of a plan upgraded from a build before the sweep ledger have no sweep member: they are measurements all the same, so the pull requests they name are measured and their conflicts are tallied.
func TestObservationsRecordedBeforeTheSweepLedgerAreMeasured(t *testing.T) {
	w := newPolicyWorld(t)
	f := w.f
	f.exec("INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at) VALUES ('dco-old', 'p', 'cand', 'hold', 'owner/repo', 'x', 'y', 'z', 1, 'git', 'parent', ?)", stamp(policyAt(0)))
	f.exec("INSERT INTO dag_conflict_observation_files (observation_id, repository, path) VALUES ('dco-old', 'owner/repo', 'a.go')")
	got := measured(t, f, "p")
	conflicts := wantPresent(t, got, 2, "conflicts_by_grade")
	if local := metric(t, conflicts, "by_grade", "local"); local["prs"] != float64(2) || local["files"] != float64(2) {
		t.Fatalf("local conflicts = %v, want both pull requests of the observation", local)
	}
	// a later, unrelated sweep adds its own nodes to the measured set and keeps the legacy ones
	w.sweep("hold", policyAt(1), "observed", 0)
	wantPresent(t, measured(t, f, "p"), 2, "conflicts_by_grade")
}
