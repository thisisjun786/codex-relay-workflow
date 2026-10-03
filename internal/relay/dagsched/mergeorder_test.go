package dagsched

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// The merge-order constraint (CRW-410, criteria c2 and c3): an observed conflict of two live nodes that no rule declared by both settles is, in the next reading, an order in which they land, carried with its
// observation in the printed reading and the recorded pass. Nothing running is stopped, and every node keeps the state, disposition and reason it had.

// orderWorld is a sweep world whose nodes D and E overlap on c.txt as local regions and are released and running, as an optimistic release leaves them; F and I declare other files.
func orderWorld(t *testing.T) *sweepWorld {
	t.Helper()
	w := newSweepWorld(t)
	for node, region := range map[string]Region{"D": {Path: "c.txt", Grade: "local"}, "E": {Path: "c.txt", Grade: "local"}, "F": {Path: "f.txt", Grade: "local"}, "I": {Path: "i.txt", Grade: "local"}} {
		w.declareRegions(node, Region{Repository: "owner/repo", Path: region.Path, Kind: "file", Change: "edit", Grade: region.Grade})
	}
	return w
}

func (w *sweepWorld) declareRegions(node string, regions ...Region) {
	w.k.t.Helper()
	if _, err := w.k.sched.DeclareRegions(context.Background(), "g", node, "parent", regions); err != nil {
		w.k.t.Fatalf("declare %s: %v", node, err)
	}
}

// measure sweeps the live nodes at the heads given (the parent's word) with no tip.
func (w *sweepWorld) measure(heads map[string]string) SweepResult {
	w.k.t.Helper()
	res, err := w.sweep(TriggerReceipt, "", "", func(in *SweepInput) { in.Heads = heads; in.Tips = nil })
	if err != nil {
		w.k.t.Fatal(err)
	}
	return res
}

func (w *sweepWorld) heads(nodes ...string) map[string]string {
	out := map[string]string{}
	for _, n := range nodes {
		out[n] = w.head[n]
	}
	return out
}

// the tables a reading and a sweep must leave as they are for a running child to be untouched
var childTables = []string{"relationships", "generations", "events", "dag_node_executions", "execution_slots", "merge_turns", "dag_acceptances"}

func (w *sweepWorld) childRows() map[string]int {
	out := map[string]int{}
	for _, table := range childTables {
		out[table] = w.k.count("SELECT COUNT(*) FROM " + table)
	}
	return out
}

func (w *sweepWorld) running(nodes ...string) {
	w.k.t.Helper()
	for _, n := range nodes {
		w.k.startNode("g", n)
		w.k.holdSlotsFor("g", n)
	}
}

// An optimistic release of an overlap, a measured conflict, and the next pass: the later node is ordered after the earlier one, with the observation it rests on, in the reading and in the recorded pass; the
// children are not stopped and the node's own state, disposition, reason and detail do not change.
func TestObservedConflictOnAnOptimisticOverlapBecomesAMergeOrderConstraint(t *testing.T) {
	w := orderWorld(t)
	k := w.k
	// before the release: E is a candidate released as local-optimistic against D
	first := k.read("g")
	if e := first.node("E"); e.Disposition != DispReady || e.Release == nil || e.Release.Rule != RuleLocalOptimistic {
		t.Fatalf("E = %+v", e)
	}
	w.running("D", "E")
	before := k.read("g")
	for _, id := range []string{"D", "E"} {
		if n := before.node(id); n.State != StateRunning || n.Disposition != DispSkip || n.Reason != SkipAlreadyOwned || n.MergeOrder != nil {
			t.Fatalf("%s before the measurement = %+v", id, n)
		}
	}
	if before.Pass.OrderConstraints != 0 {
		t.Fatalf("pass = %+v", before.Pass)
	}
	rows := w.childRows()

	res := w.measure(w.heads("D", "E"))
	pair := memberOf(res, MemberPair, "D", "E")
	if pair.Status != MemberObserved || pair.Conflicts != 1 {
		t.Fatalf("pair = %+v", pair)
	}

	after := k.read("g")
	d, e := after.node("D"), after.node("E")
	if e.MergeOrder == nil || len(e.MergeOrder.After) != 1 || len(e.MergeOrder.Before) != 0 || d.MergeOrder == nil || len(d.MergeOrder.Before) != 1 || len(d.MergeOrder.After) != 0 {
		t.Fatalf("D = %+v\nE = %+v", d.MergeOrder, e.MergeOrder)
	}
	row := e.MergeOrder.After[0]
	if row.NodeID != "D" || row.ObservationID != pair.ObservationID || row.Conflicts != 1 || !reflect.DeepEqual(row.Files, []string{"c.txt"}) || row.Grade != GradeLocal || len(row.DriftNodes) != 0 ||
		row.HeadsCurrent != HeadsCurrentUnknown || row.Lane != LaneWorking || row.Since == "" || row.Unattributed {
		t.Fatalf("E after D = %+v", row)
	}
	if back := d.MergeOrder.Before[0]; back.NodeID != "E" || back.ObservationID != pair.ObservationID || back.Lane != LaneWorking {
		t.Fatalf("D before E = %+v", back)
	}
	if !strings.Contains(e.MergeOrder.Reason, "after D") || !strings.Contains(e.MergeOrder.Reason, pair.ObservationID) || !strings.Contains(d.MergeOrder.Reason, "before E") {
		t.Errorf("reasons: %q / %q", d.MergeOrder.Reason, e.MergeOrder.Reason)
	}
	if after.Pass.OrderConstraints != 1 {
		t.Fatalf("pass = %+v", after.Pass)
	}
	// nothing is stopped: both children still run on their relationships and slots, and keep their state, disposition, reason and detail
	for _, id := range []string{"D", "E"} {
		n, was := after.node(id), before.node(id)
		if n.State != StateRunning || n.Disposition != DispSkip || n.Reason != SkipAlreadyOwned || n.Detail != was.Detail || n.Lifecycle != "" {
			t.Errorf("%s = %+v, was %+v", id, n, was)
		}
	}
	if got := w.childRows(); !reflect.DeepEqual(got, rows) {
		t.Errorf("the children's rows changed: %v, were %v", got, rows)
	}
	if got := k.count("SELECT COUNT(*) FROM relationships WHERE status <> 'active'"); got != 0 {
		t.Errorf("%d relationships left active", got)
	}
	if after.InputDigest == before.InputDigest {
		t.Error("the input digest does not cover the constraint")
	}

	// the printed reading
	doc := jsonOf(t, after)
	if doc["pass"].(map[string]any)["order_constraints"] != float64(1) {
		t.Errorf("pass = %v", doc["pass"])
	}
	printed := nodeJSON(t, doc["nodes"], "E")["merge_order"].(map[string]any)
	line := printed["after"].([]any)[0].(map[string]any)
	if line["node_id"] != "D" || line["observation_id"] != pair.ObservationID || line["conflicts"] != float64(1) || line["grade"] != "local" || line["lane"] != "working" ||
		!reflect.DeepEqual(line["files"], []any{"c.txt"}) || printed["reason"] == "" || printed["tip"] != nil {
		t.Errorf("printed = %v", printed)
	}
	if _, has := nodeJSON(t, doc["nodes"], "F")["merge_order"]; has {
		t.Error("a node outside the pair has a constraint")
	}
	// two readings of one store state are byte-identical
	if a, b := emitted(t, k.read("g")), emitted(t, k.read("g")); a != b {
		t.Error("two readings differ")
	}
	// the recorded pass keeps the constraint
	recorded, seq := k.recordPass("g")
	if seq != 1 || recorded.node("E").MergeOrder == nil {
		t.Fatalf("recorded = %d %+v", seq, recorded.node("E"))
	}
	var stored []any
	if err := json.Unmarshal([]byte(k.passRows("g")[0].dispositions), &stored); err != nil {
		t.Fatal(err)
	}
	kept := nodeJSON(t, stored, "E")["merge_order"].(map[string]any)
	if kept["after"].([]any)[0].(map[string]any)["observation_id"] != pair.ObservationID || kept["reason"] == "" {
		t.Errorf("the pass record holds %v", kept)
	}
	if _, has := nodeJSON(t, stored, "D")["merge_order"].(map[string]any)["before"]; !has {
		t.Error("the pass record holds no merge_order for D")
	}
	// progress and the restart reading do not change what they say of a constrained node
	if progress, err := k.sched.ReadProgress(context.Background(), "g"); err != nil {
		t.Fatalf("progress: %v", err)
	} else {
		for _, n := range progress.Nodes {
			if n.NodeID == "E" && (n.Reason != SkipAlreadyOwned || strings.Contains(n.Detail, "merge")) {
				t.Errorf("progress of E = %+v", n)
			}
		}
	}
}

// No constraint where nothing asks for one: a clean latest measurement, a conflict every covering pair of regions settles by a rule, no measurement at all. One that a local pair of regions leaves open is kept.
func TestMergeOrderNeedsAConflictNoRuleSettles(t *testing.T) {
	union := func(path string) Region {
		return Region{Repository: "owner/repo", Path: path, Kind: "file", Change: "edit", Grade: "mechanical", Rule: "union"}
	}
	symbol := Region{Repository: "owner/repo", Path: "c.txt", Kind: "symbol", Key: "f", Change: "edit", Grade: "local"}
	cases := []struct {
		name     string
		declare  []Region // what D and E both declare of c.txt
		eHead    string   // the node whose head E is measured at
		wantPair bool
		drift    []string
	}{
		{"a local overlap that conflicts", []Region{{Repository: "owner/repo", Path: "c.txt", Kind: "file", Change: "edit", Grade: "local"}}, "E", true, nil},
		{"a clean pair", []Region{{Repository: "owner/repo", Path: "c.txt", Kind: "file", Change: "edit", Grade: "local"}}, "F", false, nil},
		{"every covering pair of regions is mechanical", []Region{union("c.txt")}, "E", false, nil},
		{"a mechanical file with a local symbol inside it", []Region{union("c.txt"), symbol}, "E", true, nil},
		{"a file no declaration covers is drift and keeps the constraint", []Region{{Repository: "owner/repo", Path: "other.txt", Kind: "file", Change: "edit", Grade: "local"}}, "E", true, []string{"D", "E"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newSweepWorld(t)
			w.declareRegions("D", c.declare...)
			w.declareRegions("E", c.declare...)
			w.declareRegions("F", Region{Repository: "owner/repo", Path: "f.txt", Kind: "file", Change: "edit", Grade: "local"})
			w.declareRegions("I", Region{Repository: "owner/repo", Path: "i.txt", Kind: "file", Change: "edit", Grade: "local"})
			w.running("D", "E", "F")
			if got := w.k.read("g"); got.Pass.OrderConstraints != 0 {
				t.Fatalf("a constraint with no measurement: %+v", got.Pass)
			}
			res := w.measure(map[string]string{"D": w.head["D"], "E": w.head[c.eHead], "F": w.head["F"]})
			if m := memberOf(res, MemberPair, "D", "E"); m.Status != MemberObserved || (m.Conflicts == 1) != (c.eHead == "E") {
				t.Fatalf("pair = %+v", m)
			}
			reading := w.k.read("g")
			if got := reading.Pass.OrderConstraints; (got == 1) != c.wantPair {
				t.Fatalf("constraints = %d, want pair %v", got, c.wantPair)
			}
			if c.wantPair {
				if row := reading.node("E").MergeOrder.After[0]; !reflect.DeepEqual(row.DriftNodes, c.drift) {
					t.Errorf("drift nodes = %v, want %v", row.DriftNodes, c.drift)
				}
			}
		})
	}
}
