package dagsched

import (
	"context"
	"fmt"
	"reflect"
	"testing"
)

// The order a constraint puts two nodes in follows the merge lane, and the constraint follows the measurements and the holders: the latest measurement of the current heads decides, a node that landed
// drops out, and a store that has none of the tables reads as it always did (CRW-410, criteria c2 and c3).

func ids(rows []OrderRow) []string {
	out := []string{}
	for _, r := range rows {
		out = append(out, r.NodeID)
	}
	return out
}

func local(path string) Region {
	return Region{Repository: "owner/repo", Path: path, Kind: "file", Change: "edit", Grade: "local"}
}

// The latest measurement of a pair decides: a clean one clears the constraint, and the conflicting heads measured again (a replay of the first observation row, which is older than the clean one) constrain
// again, because the member of the latest sweep is what orders the measurements.
func TestMergeOrderFollowsTheLatestMeasurementOfAPair(t *testing.T) {
	w := orderWorld(t)
	w.running("D", "E")
	first := w.measure(w.heads("D", "E"))
	conflict := memberOf(first, MemberPair, "D", "E")
	if got := w.k.read("g").Pass.OrderConstraints; got != 1 {
		t.Fatalf("constraints = %d", got)
	}
	// E is measured at a head that merges cleanly: the latest measurement is clean
	w.measure(map[string]string{"D": w.head["D"], "E": w.head["F"]})
	if got := w.k.read("g"); got.Pass.OrderConstraints != 0 || got.node("E").MergeOrder != nil || got.node("D").MergeOrder != nil {
		t.Fatalf("a clean latest measurement left a constraint: %+v", got.Pass)
	}
	// the conflicting heads again: the observation row is the first one, replayed, and it is the latest measurement
	again := w.measure(w.heads("D", "E"))
	if m := memberOf(again, MemberPair, "D", "E"); m.Status != MemberReplayed || m.ObservationID != conflict.ObservationID {
		t.Fatalf("again = %+v", m)
	}
	reading := w.k.read("g")
	if reading.Pass.OrderConstraints != 1 || reading.node("E").MergeOrder == nil || reading.node("E").MergeOrder.After[0].ObservationID != conflict.ObservationID {
		t.Fatalf("a replayed conflict does not constrain: %+v", reading.node("E").MergeOrder)
	}
}

// The node that lands first no longer holds its regions once it landed: the constraint is gone from both.
func TestMergeOrderDropsOutWhenTheEarlierNodeLands(t *testing.T) {
	w := orderWorld(t)
	k := w.k
	k.exec("INSERT INTO dag_conflict_observations (observation_id, plan_id, left_node_id, right_node_id, repository, left_head, right_head, base_sha, conflict_count, method, observed_by, observed_at)" +
		" SELECT 'unused', 'g', 'D', 'E', 'x', 'a', 'b', 'c', 0, 'm', 'p', 't' WHERE 0")
	w.running("E")
	a := k.acceptNode("g", "D", acceptOpts{HeadSHA: w.head["D"], PR: 20, Forge: "owner/repo", Repository: k.repo.path})
	w.measure(w.heads("D", "E"))
	reading := k.read("g")
	if row := reading.node("E").MergeOrder.After; len(row) != 1 || row[0].NodeID != "D" || row[0].Lane != LaneAccepted {
		t.Fatalf("E = %+v", reading.node("E").MergeOrder)
	}
	// D lands: its head is contained in dev, the parent marked it merged, the landing is observed
	k.repo.git("merge", "-q", "-s", "ours", "-m", "land D", "b-D")
	k.mark(a)
	if res, err := k.sched.ObserveIntegration(context.Background(), "g", "D", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev"}}); err != nil || !res.Integrated {
		t.Fatalf("landing = %v %+v", err, res)
	}
	reading = k.read("g")
	if reading.node("D").State != StateIntegrated || reading.node("E").MergeOrder != nil || reading.Pass.OrderConstraints != 0 || reading.node("D").MergeOrder != nil {
		t.Fatalf("after the landing: D=%+v E=%+v pass=%+v", reading.node("D"), reading.node("E").MergeOrder, reading.Pass)
	}
}

// Which node lands first is the merge lane's order: an open merge turn by requested_at and turn id, then an accepted result by the time it was accepted, then everything else that holds regions by when its work
// began, or for a node with no child yet by the time its release was decided. A paused accepted node is not in the lane.
func TestMergeOrderFollowsTheMergeLane(t *testing.T) {
	turn := func(w *sweepWorld, id, node, requested string) { turnOf(w, id, "rel-g-"+node, w.head[node], requested) }
	first := func(w *sweepWorld, nodes ...string) []string { // the node every other is After, from the reading
		w.measure(w.heads(nodes...))
		r := w.k.read("g")
		var after []string
		for _, n := range nodes {
			if mo := r.node(n).MergeOrder; mo != nil && len(mo.After) > 0 {
				after = append(after, n+"<"+fmt.Sprint(ids(mo.After)))
			}
		}
		return after
	}
	t.Run("an accepted result goes before a working node that began earlier", func(t *testing.T) {
		w := orderWorld(t)
		w.running("E") // began first
		w.k.acceptNode("g", "D", acceptOpts{HeadSHA: w.head["D"], PR: 20, Forge: "owner/repo", Repository: w.k.repo.path})
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("order = %v", got)
		}
		if row := w.k.read("g").node("D").MergeOrder.Before[0]; row.Lane != LaneWorking || row.NodeID != "E" {
			t.Fatalf("D before = %+v", row)
		}
	})
	t.Run("two accepted results go by the time they were accepted", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("order = %v", got)
		}
	})
	t.Run("the merge turn requested first goes first, out of acceptance order", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		turn(w, "turn-e", "E", "2026-10-02T01:00:00Z")
		turn(w, "turn-d", "D", "2026-10-02T02:00:00Z")
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"D<[E]"}) {
			t.Fatalf("order = %v", got)
		}
		if row := w.k.read("g").node("D").MergeOrder.After[0]; row.Lane != LaneTurn || row.Since != "2026-10-02T01:00:00Z" {
			t.Fatalf("D after = %+v", row)
		}
	})
	t.Run("a turn of another relationship for the same commit does not order this plan's nodes", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		turnOf(w, "turn-elsewhere", "rel-another-project", w.head["E"], "2026-10-01T00:00:00Z")
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("order = %v", got)
		}
		if row := w.k.read("g").node("E").MergeOrder.After[0]; row.Lane != LaneAccepted {
			t.Fatalf("D is in the lane %q, want accepted", row.Lane)
		}
	})
	t.Run("a node the plan paused is working whatever it is accepted as, and so is every node of a paused plan", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("before the pause = %v", got)
		}
		w.k.putPlan("g", 2, "g-r3", lifeOp("pause_node", "D"))
		after := w.k.read("g")
		if d := after.node("D"); lifecycleOf(d) != "paused" || d.MergeOrder == nil || ids(d.MergeOrder.After) == nil || len(d.MergeOrder.After) != 1 || d.MergeOrder.After[0].NodeID != "E" || d.MergeOrder.After[0].Lane != LaneAccepted {
			t.Fatalf("the paused D = %+v", d.MergeOrder)
		}
		if e := after.node("E"); e.MergeOrder == nil || len(e.MergeOrder.After) != 0 || e.MergeOrder.Before[0].Lane != LaneWorking {
			t.Fatalf("E = %+v", e.MergeOrder)
		}
		w.k.putPlan("g", 3, "g-r4", planOp("pause_plan"))
		if e := w.k.read("g").node("E"); e.MergeOrder == nil || len(e.MergeOrder.Before) != 1 && len(e.MergeOrder.After) != 1 || (len(e.MergeOrder.After) == 1 && e.MergeOrder.After[0].Lane != LaneWorking) {
			t.Fatalf("a paused plan: E = %+v", e.MergeOrder)
		}
	})
	t.Run("an archived node can still land: it keeps its turn and its place", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		turn(w, "turn-d", "D", "2026-10-02T01:00:00Z")
		turn(w, "turn-e", "E", "2026-10-02T02:00:00Z")
		w.k.putPlan("g", 2, "g-r3", lifeOp("archive_node", "D"))
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("with turns = %v", got)
		}
		if row := w.k.read("g").node("E").MergeOrder.After[0]; row.Lane != LaneTurn {
			t.Fatalf("archived D is in the lane %q, want turn", row.Lane)
		}
	})
	t.Run("an archived accepted node with no turn is still an accepted result", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		w.k.putPlan("g", 2, "g-r3", lifeOp("archive_node", "D"))
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("order = %v", got)
		}
		if row := w.k.read("g").node("E").MergeOrder.After[0]; row.Lane != LaneAccepted {
			t.Fatalf("archived D is in the lane %q, want accepted", row.Lane)
		}
	})
	t.Run("a cancelled node cannot merge: it is working", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		w.k.putPlan("g", 2, "g-r3", lifeOp("cancel_node", "D"))
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"D<[E]"}) {
			t.Fatalf("order = %v", got)
		}
	})
	t.Run("an accepted result that is no longer the node's current one is working", func(t *testing.T) {
		w := orderWorld(t)
		w.accept("D", "E")
		w.k.supersedeReport("rel-g-D", "D", "g", "again") // a newer report of D's generation: its acceptance is history until the new one is accepted
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"D<[E]"}) {
			t.Fatalf("order = %v", got)
		}
		if row := w.k.read("g").node("D").MergeOrder.After[0]; row.Lane != LaneAccepted || row.NodeID != "E" {
			t.Fatalf("D after = %+v", row)
		}
	})
	t.Run("a paused accepted node is working and goes after an accepted one that began later", func(t *testing.T) {
		w := orderWorld(t)
		w.k.acceptNode("g", "D", acceptOpts{HeadSHA: w.head["D"], PR: 20, Forge: "owner/repo", Repository: w.k.repo.path, Status: "paused"})
		w.k.acceptNode("g", "E", acceptOpts{HeadSHA: w.head["E"], PR: 21, Forge: "owner/repo", Repository: w.k.repo.path})
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"D<[E]"}) {
			t.Fatalf("order = %v", got)
		}
	})
	t.Run("a node that holds regions before it has a child is ordered by its release", func(t *testing.T) {
		w := orderWorld(t)
		w.k.exec("INSERT INTO dag_releases (plan_id, node_id, manifest_digest, managed_request_id, coordinator_epoch, decided_at) VALUES ('g', 'D', 'm1', 'req-1', 0, '2026-10-01T00:00:00Z')")
		w.running("E")
		if got := w.k.read("g"); got.node("D").State != StateReleasing {
			t.Fatalf("D = %+v", got.node("D"))
		}
		if got := first(w, "D", "E"); !reflect.DeepEqual(got, []string{"E<[D]"}) {
			t.Fatalf("order = %v", got)
		}
		if row := w.k.read("g").node("E").MergeOrder.After[0]; row.Since != "2026-10-01T00:00:00Z" {
			t.Fatalf("E after = %+v", row)
		}
	})
}

// Three nodes that conflict pairwise are ordered by one key, so the constraints cannot form a cycle: the last one lists the two before it, in landing order.
func TestMergeOrderOfThreeNodesIsAcyclic(t *testing.T) {
	w := newSweepWorld(t)
	for _, node := range []string{"D", "E", "I"} {
		w.declareRegions(node, local("c.txt"))
	}
	w.declareRegions("F", local("f.txt"))
	w.running("D", "E", "I")
	w.measure(w.heads("D", "E", "I"))
	r := w.k.read("g")
	if r.Pass.OrderConstraints != 3 {
		t.Fatalf("pairs = %d", r.Pass.OrderConstraints)
	}
	got := map[string][2][]string{}
	for _, n := range []string{"D", "E", "I"} {
		mo := r.node(n).MergeOrder
		got[n] = [2][]string{ids(mo.After), ids(mo.Before)}
	}
	want := map[string][2][]string{"D": {{}, {"E", "I"}}, "E": {{"D"}, {"I"}}, "I": {{"D", "E"}, {}}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("order = %v, want %v", got, want)
	}
}

// A head's own conflict with the tip is the base refresh it needs whatever lands first; the latest measurement decides, and a clean head has none.
func TestMergeOrderCarriesTheHeadsConflictWithTheTip(t *testing.T) {
	w := orderWorld(t)
	w.running("D", "E")
	w.measureAt(map[string]string{"D": w.head["D"], "E": w.head["F"]})
	r := w.k.read("g")
	tip := r.node("D").MergeOrder
	if tip == nil || tip.Tip == nil || len(tip.After)+len(tip.Before) != 0 || tip.Tip.Conflicts != 1 || tip.Tip.TipSHA != w.tip || tip.Tip.HeadSource != HeadExplicit || !reflect.DeepEqual(tip.Tip.Files, []string{"c.txt"}) {
		t.Fatalf("D = %+v", tip)
	}
	if r.node("E").MergeOrder != nil || r.Pass.OrderConstraints != 0 {
		t.Fatalf("E = %+v pass = %+v", r.node("E").MergeOrder, r.Pass)
	}
	// D refreshed its base: its head contains the tip, and the latest measurement is clean
	w.measureAt(map[string]string{"D": w.tip, "E": w.head["F"]})
	if got := w.k.read("g").node("D").MergeOrder; got != nil {
		t.Fatalf("a clean head still has %+v", got)
	}
}

func (w *sweepWorld) measureAt(heads map[string]string) SweepResult {
	w.k.t.Helper()
	res, err := w.sweep(TriggerReceipt, "", "", func(in *SweepInput) { in.Heads = heads })
	if err != nil {
		w.k.t.Fatal(err)
	}
	return res
}

// heads_current says whether the measurement was made at the heads the store holds now: yes for two accepted results measured as accepted, no when a stored head differs, unknown when a head is not stored.
func TestMergeOrderSaysWhetherTheMeasurementIsAtTheCurrentHeads(t *testing.T) {
	w := orderWorld(t)
	w.accept("D", "E")
	w.measure(nil)
	if row := w.k.read("g").node("E").MergeOrder.After[0]; row.HeadsCurrent != HeadsCurrentYes {
		t.Fatalf("at the accepted heads: %+v", row)
	}
	w.measure(map[string]string{"D": w.head["I"]})
	if row := w.k.read("g").node("E").MergeOrder.After[0]; row.HeadsCurrent != HeadsCurrentNo {
		t.Fatalf("at another head than D's accepted one: %+v", row)
	}
}

// A store whose zone predates the tables (a read-only open of an older store is that state) reads without a constraint and without an error.
func TestMergeOrderReadsAStoreWithoutTheSweepTables(t *testing.T) {
	w := orderWorld(t)
	w.running("D", "E")
	w.measure(w.heads("D", "E"))
	if got := w.k.read("g").Pass.OrderConstraints; got != 1 {
		t.Fatalf("constraints = %d", got)
	}
	for _, table := range []string{"dag_conflict_sweep_members", "dag_conflict_sweeps", "dag_conflict_drift", "dag_tip_conflict_observation_files", "dag_tip_conflict_observations"} {
		w.k.exec("DROP TABLE " + table)
	}
	r := w.k.read("g")
	if r.Pass.OrderConstraints != 0 || r.node("E").MergeOrder != nil || r.node("E").State != StateRunning {
		t.Fatalf("reading = %+v", r.node("E"))
	}
}

// A constraint does not get in the way of a release: the release path asks for the reading under its lock and the node released is not the constrained one.
func TestReleaseWorksWhileAConstraintExists(t *testing.T) {
	w := orderWorld(t)
	k := w.k
	k.sched.Tips = k.tips
	k.tips.sha = w.tip
	w.running("D", "E")
	w.measure(w.heads("D", "E"))
	if k.read("g").Pass.OrderConstraints != 1 {
		t.Fatal("no constraint")
	}
	if res, err := k.release("g", "F"); err != nil || res.ManifestDigest == "" {
		t.Fatalf("release F = %v %+v", err, res)
	}
}

// turnOf is an open merge turn of a relationship for a commit.
func turnOf(w *sweepWorld, id, relationship, head, requested string) {
	w.k.t.Helper()
	w.k.exec("INSERT INTO merge_turns (turn_id, target_key, repository, base_ref, project_key, holder_task_id, holder_host_id, relationship_id, candidate_head, state, tenure, requested_at, updated_at) VALUES (?, 'tgt', 'owner/repo', 'dev', 'P-TEST', ?, 'host', ?, ?, 'waiting', 1, ?, ?)",
		id, "holder-"+id, relationship, head, requested, requested)
}
