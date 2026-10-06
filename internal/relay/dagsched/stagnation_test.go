package dagsched

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// stagnatingPlan is the plan the stagnation tests share: an implementation node I with a landing target (the integrated edge to J), so
// the same fixture can also show a node that landed.
func stagnatingPlan(f *fixture, plan string) {
	f.t.Helper()
	f.putPlan(plan, 0, plan+"-r1",
		addNode("I", dag.NodeImplementation), addNode("J", dag.NodeNonPR),
		addEdge("ij", "I", "J", dag.EdgeIntegrated, nil))
}

// addCorrection appends one correction generation of a node's relationship and, unless handOpened, the needs_changes ruling on the
// generation before it that opened it: the ruling event carries the findings the relay's verdict writer stores, the restoration block
// being the one entry declared with restoration true. A generation opened by hand has no ruling and no findings at all.
func (f *fixture) addCorrection(plan, node, rid string, gen int64, findings []map[string]any, handOpened bool) {
	f.t.Helper()
	now := f.clock()
	before := gen - 1
	if !handOpened {
		raw, err := json.Marshal(findings)
		if err != nil {
			f.t.Fatal(err)
		}
		event := fmt.Sprintf("evt-rule-%s-%d", rid, gen)
		f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
			" VALUES (?, ?, ?, ?, 'ready_for_review', 'child', 'child', 'turn-r', 'completed', '{}', 'final', ?, ?)", event, rid, before, dig(fmt.Sprintf("revision %s %d", rid, before)), now, now)
		f.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?, '{}', 'needs_changes', ?, 'verdict-turn', ?)", event, gen, now)
		f.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, findings, currency, ack_evidence, recorded_at) VALUES (?, NULL, '{}', ?, 'current', '{}', ?)", event, string(raw), now)
	}
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, ?, ?, 'bound', 'turn-r', 'needs_changes_revision', ?, ?)",
		rid, gen, fmt.Sprintf("revision-%s-%d", rid, gen), now, now)
	f.exec("UPDATE relationships SET execution_generation = ? WHERE relationship_id = ?", gen, rid)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?, ?, ?, ?, ?, 'correction', NULL)",
		plan, node, rid, gen, dig(fmt.Sprintf("manifest %s %d", rid, gen)))
}

// rulingFinding is one ruling's findings: the criterion entries, with the restoration block's note differing every round (it carries the
// generation's manifest digest), so the test proves the identity is taken over the entries that are not the restoration block.
func rulingFinding(note string, round int64) []map[string]any {
	return []map[string]any{
		{"id": "c1", "verdict": "needs_changes", "note": note},
		{"id": "c2", "verdict": "needs_changes", "note": fmt.Sprintf("Correction generation %d of CRW-I: the manifest at /tmp/m-%d.json", round, round), "restoration": true},
	}
}

// zoneRows is the whole zone's row count, so a reading that wrote anything shows up as a difference.
func (f *fixture) zoneRows() map[string]int {
	f.t.Helper()
	out := map[string]int{}
	for _, table := range []string{"dag_node_executions", "dag_merge_checks", "dag_acceptance_revalidations", "dag_passes", "generations", "verdicts", "verdict_context", "events"} {
		out[table] = f.count("SELECT COUNT(*) FROM " + table)
	}
	return out
}

func (f *fixture) stagnationOf(plan, node string) *Stagnation {
	f.t.Helper()
	return f.read(plan).node(node).Stagnation
}

// TestARepeatedFindingRaisesTheStagnationCountAndOpensTheNextRung is the red test the issue body names: two corrections carrying the
// same finding raise the count to 2 and name edit_packet, a third names split_node, and the controls reset the count, ignore a
// generation opened by hand, read no object for a node that landed, and read a repeated failed check as its own cause.
func TestARepeatedFindingRaisesTheStagnationCountAndOpensTheNextRung(t *testing.T) {
	t.Run("two corrections with the same finding reach the threshold and name edit_packet, a third names split_node", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		rid := f.startNode("sp", "I")
		f.addCorrection("sp", "I", rid, 2, rulingFinding("fix the same thing", 2), false)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("one correction reads %+v, want no stagnation object below the threshold of 2", st)
		}
		f.addCorrection("sp", "I", rid, 3, rulingFinding("fix the same thing", 3), false)
		st := f.stagnationOf("sp", "I")
		if st == nil {
			t.Fatal("two corrections with the same finding read no stagnation object")
		}
		if st.Count != 2 || st.Cause != CauseRepeatedFinding || st.Rung != RungEditPacket || st.NodeID != "I" {
			t.Fatalf("two corrections read %+v, want count 2, repeated_finding and edit_packet", st)
		}
		if st.FindingDigest == "" || st.LastEventID == "" || st.Next == "" {
			t.Fatalf("the object carries no identity, event or next action: %+v", st)
		}
		f.addCorrection("sp", "I", rid, 4, rulingFinding("fix the same thing", 4), false)
		st = f.stagnationOf("sp", "I")
		if st == nil || st.Count != 3 || st.Rung != RungSplitNode {
			t.Fatalf("three corrections read %+v, want count 3 and split_node", st)
		}
	})

	t.Run("a different finding resets the count", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		rid := f.startNode("sp", "I")
		f.addCorrection("sp", "I", rid, 2, rulingFinding("the first thing", 2), false)
		f.addCorrection("sp", "I", rid, 3, rulingFinding("the first thing", 3), false)
		if st := f.stagnationOf("sp", "I"); st == nil || st.Count != 2 {
			t.Fatalf("two equal findings read %+v, want count 2", st)
		}
		f.addCorrection("sp", "I", rid, 4, rulingFinding("a different thing", 4), false)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("a different finding reads %+v, want the run reset to one and no object", st)
		}
	})

	t.Run("a generation the coordinator opened by hand raises no repeated-finding count", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		rid := f.startNode("sp", "I")
		f.addCorrection("sp", "I", rid, 2, rulingFinding("the same thing", 2), false)
		f.addCorrection("sp", "I", rid, 3, rulingFinding("the same thing", 3), false)
		if st := f.stagnationOf("sp", "I"); st == nil || st.Count != 2 {
			t.Fatalf("two equal findings read %+v, want count 2", st)
		}
		f.addCorrection("sp", "I", rid, 4, nil, true)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("a generation opened by hand reads %+v, want the run broken and no object", st)
		}
		f.addCorrection("sp", "I", rid, 5, rulingFinding("the same thing", 5), false)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("one ruling after a generation opened by hand reads %+v, want no object", st)
		}
	})

	t.Run("a node that landed reads no stagnation object", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		rid := a.Acceptance.RelationshipID
		f.addCorrection("sp", "I", rid, 2, rulingFinding("the same thing", 2), false)
		f.addCorrection("sp", "I", rid, 3, rulingFinding("the same thing", 3), false)
		if st := f.stagnationOf("sp", "I"); st == nil || st.Count != 2 {
			t.Fatalf("the node before it landed reads %+v, want count 2", st)
		}
		f.integrate(a, "owner/repo", "dev", true, true)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("a node that landed reads %+v, want no stagnation object", st)
		}
	})

	t.Run("a repeated failed required check is its own cause", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		failed := failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}})
		body := EvidenceBody{Required: []string{"dev-gate"}}.JSON()
		digest := dig("evidence")
		f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
			" VALUES ('mc-1', ?, 1, ?, ?, 'tip', ?, ?, ?, 1, 'retry_same_sha', 'failed once', 't')", a.Acceptance.AcceptanceID, head1, head1, digest, body, failed)
		f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
			" VALUES ('mc-2', ?, 2, ?, ?, 'tip', ?, ?, ?, 2, 'evicted', 'failed again', 't')", a.Acceptance.AcceptanceID, head1, head1, digest, body, failed)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedCheckFailure || st.Rung != RungEditPacket || st.FindingDigest != "" {
			t.Fatalf("two rows failing the same required check read %+v, want count 2 and repeated_check_failure", st)
		}
	})
}

// TestTheStagnationLadderStopsAtFullReplan is the second red test the issue body names: the rung stops at the last one, a reading past
// it stays there, and the reading is a function of the store (two reads agree, byte for byte, and nothing is written).
func TestTheStagnationLadderStopsAtFullReplan(t *testing.T) {
	f := newFixture(t)
	f.projectParent()
	stagnatingPlan(f, "sp")
	rid := f.startNode("sp", "I")
	// generation 2 is the first correction (one finding, below the threshold); each later generation raises the run by one, and the rung
	// stops at the last one
	rungs := map[int64]struct {
		count int
		rung  string
	}{3: {2, RungEditPacket}, 4: {3, RungSplitNode}, 5: {4, RungNeighbourRepair}, 6: {5, RungFullReplan}, 7: {6, RungFullReplan}, 9: {8, RungFullReplan}}
	var last *Stagnation
	for gen := int64(2); gen <= 9; gen++ {
		f.addCorrection("sp", "I", rid, gen, rulingFinding("the same thing", gen), false)
		want, checked := rungs[gen]
		if !checked {
			continue
		}
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != want.count || st.Rung != want.rung {
			t.Fatalf("generation %d reads %+v, want count %d and rung %s", gen, st, want.count, want.rung)
		}
		last = st
	}
	if last == nil || last.Rung != RungFullReplan || last.Next != "" {
		t.Fatalf("the ladder's last rung reads %+v, want the rung at full_replan and no further rung named", last)
	}
	before := f.zoneRows()
	first, second := f.read("sp"), f.read("sp")
	if string(marshal(first.Object())) != string(marshal(second.Object())) || first.InputDigest != second.InputDigest {
		t.Fatalf("two readings of one state differ: %s %s", marshal(first.Object()), marshal(second.Object()))
	}
	if after := f.zoneRows(); fmt.Sprint(before) != fmt.Sprint(after) {
		t.Fatalf("the reading wrote rows: %v -> %v", before, after)
	}
	// a rung below the last still names the one after it, and the count's rung follows the run of equal findings
	t.Run("neighbour_repair names full_replan next", func(t *testing.T) {
		g := newFixture(t)
		g.projectParent()
		stagnatingPlan(g, "sp2")
		rid2 := g.startNode("sp2", "I")
		for gen := int64(2); gen <= 5; gen++ {
			g.addCorrection("sp2", "I", rid2, gen, rulingFinding("the same thing", gen), false)
		}
		if st := g.stagnationOf("sp2", "I"); st == nil || st.Count != 4 || st.Rung != RungNeighbourRepair || st.Next != RungFullReplan {
			t.Fatalf("count 4 reads %+v, want neighbour_repair and full_replan next", st)
		}
	})
}
