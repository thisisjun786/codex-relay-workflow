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
// generation before it that opened it. It is addCorrectionFrom with the ruling on generation-1, which is what a store with no withdrawn
// generation holds.
func (f *fixture) addCorrection(plan, node, rid string, gen int64, findings []map[string]any, handOpened bool) {
	f.t.Helper()
	f.addCorrectionFrom(plan, node, rid, gen, gen-1, findings, handOpened)
}

// addCorrectionFrom appends one correction generation of a node's relationship and, unless handOpened, the needs_changes ruling on the
// generation it follows (from): the ruling event carries the findings the relay's verdict writer stores, the restoration block being the
// one entry declared with restoration true. A generation opened by hand has no ruling and no findings at all. A generation that follows a
// withdrawn one is ruled on the nearest live generation below it, which is what from names here.
func (f *fixture) addCorrectionFrom(plan, node, rid string, gen, from int64, findings []map[string]any, handOpened bool) {
	f.t.Helper()
	now := f.clock()
	if !handOpened {
		raw, err := json.Marshal(findings)
		if err != nil {
			f.t.Fatal(err)
		}
		event := fmt.Sprintf("evt-rule-%s-%d", rid, gen)
		f.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
			" VALUES (?, ?, ?, ?, 'ready_for_review', 'child', 'child', 'turn-r', 'completed', '{}', 'final', ?, ?)", event, rid, from, dig(fmt.Sprintf("revision %s %d", rid, from)), now, now)
		f.exec("INSERT INTO verdicts (event_id, record, verdict, next_generation, verdict_turn_id, decided_at) VALUES (?, '{}', 'needs_changes', ?, 'verdict-turn', ?)", event, gen, now)
		f.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, findings, currency, ack_evidence, recorded_at) VALUES (?, NULL, '{}', ?, 'current', '{}', ?)", event, string(raw), now)
	}
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, ?, ?, 'bound', 'turn-r', 'needs_changes_revision', ?, ?)",
		rid, gen, fmt.Sprintf("revision-%s-%d", rid, gen), now, now)
	f.exec("UPDATE relationships SET execution_generation = ? WHERE relationship_id = ?", gen, rid)
	f.exec("INSERT INTO dag_node_executions (plan_id, node_id, relationship_id, execution_generation, manifest_digest, kind, managed_request_id) VALUES (?, ?, ?, ?, ?, 'correction', NULL)",
		plan, node, rid, gen, dig(fmt.Sprintf("manifest %s %d", rid, gen)))
}

// withdrawGeneration records a generation the coordinator opened by hand and withdrew before it was sent: its generations row stays, the
// withdrawal names the generation the relationship goes back to, and the number is never reused.
func (f *fixture) withdrawGeneration(rid, plan, node string, gen, restored int64) {
	f.t.Helper()
	now := f.clock()
	request := fmt.Sprintf("revision-hand-%s-%d", rid, gen)
	f.exec("INSERT INTO generations (relationship_id, execution_generation, dispatch_request_id, anchor_state, dispatch_turn_id, reason, opened_at, bound_at) VALUES (?, ?, ?, 'bound', 'turn-r', 'correction', ?, ?)",
		rid, gen, request, now, now)
	f.exec("INSERT INTO dag_generation_withdrawals (relationship_id, execution_generation, plan_id, node_id, dispatch_request_id, opened_reason, restored_generation, reason, withdrawn_by_task_id, coordinator_epoch, withdrawn_at)"+
		" VALUES (?, ?, ?, ?, ?, 'correction', ?, 'withdrawn before sending', 'parent', 0, ?)", rid, gen, plan, node, request, restored, now)
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

// stagnatingHead2 is a second head a stagnation test moves an acceptance onto with a recorded base refresh.
const stagnatingHead2 = "2222222222222222222222222222222222222222"

// mergeCheckRow appends one merge-check row of an acceptance the way the merge-check writer appends it: the head the acceptance stood on when the row was written (head_sha, which is the stand head
// after a recorded base refresh), the head the pull request showed (the same here), the failed required checks and the outcome. Writing it directly lets a test put a history there.
func (f *fixture) mergeCheckRow(acceptance, id string, seq int, head, failed string, round int, outcome string) {
	f.t.Helper()
	body := EvidenceBody{Required: []string{"dev-gate"}}.JSON()
	f.exec("INSERT INTO dag_merge_checks (check_id, acceptance_id, check_seq, head_sha, observed_head_sha, base_tip_sha, checks_digest, evidence_json, failed_required_json, round_no, outcome, reason, recorded_at)"+
		" VALUES (?, ?, ?, ?, ?, 'tip', ?, ?, ?, ?, ?, 'test', 't')", id, acceptance, seq, head, head, dig("evidence "+id), body, failed, round, outcome)
}

// recordStandHead records a base refresh that moves an acceptance onto another head, the way dag-base-refresh does: the acceptance keeps its row and its head, and the head it STANDS on becomes the
// record's. The reading follows the head the acceptance stands on now.
func (f *fixture) recordStandHead(a accepted, head string) {
	f.t.Helper()
	generation := a.Acceptance.ExecutionGeneration + 1
	revision := dig("revision refresh " + head)
	id := refreshDigest(a.Acceptance.AcceptanceID, a.Acceptance.RelationshipID, generation, a.Event, revision, head, "owner/repo", "dev", "tip", "{}", "[]")
	f.exec("INSERT INTO dag_base_refreshes (refresh_id, acceptance_id, refresh_seq, relationship_id, execution_generation, event_id, revision_hash, head_sha, base_repository, base_ref, base_tip_sha, proof_json, resolved_paths_json, recorded_by_task_id, coordinator_epoch, recorded_at)"+
		" VALUES (?, ?, 1, ?, ?, ?, ?, ?, 'owner/repo', 'dev', 'tip', '{}', '[]', 'parent', 0, 't')",
		id, a.Acceptance.AcceptanceID, a.Acceptance.RelationshipID, generation, a.Event, revision, head)
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
		// the retry round's failure is one attempt and the eviction after it another: two distinct failed attempts of the same required check
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedCheckFailure || st.Rung != RungEditPacket || st.FindingDigest != "" {
			t.Fatalf("two distinct failed attempts of the same required check read %+v, want count 2 and repeated_check_failure", st)
		}
		// a head accepted again and evicted again is the same check a third time: the round restarts, the attempt does not
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-3", 3, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 3}}), 1, OutcomeEvicted)
		if st := f.stagnationOf("sp", "I"); st == nil || st.Count != 3 || st.Cause != CauseRepeatedCheckFailure || st.Rung != RungSplitNode {
			t.Fatalf("the same required check failing a third time reads %+v, want count 3 and split_node", st)
		}
		// a different required check failing is a different failure and starts the run over
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-4", 4, head1, failuresJSON([]failure{{Name: "lint", Run: "4", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("a different required check reads %+v, want the run reset and no object", st)
		}
	})

	t.Run("the longer run decides the cause", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		rid := a.Acceptance.RelationshipID
		for gen := int64(2); gen <= 4; gen++ {
			f.addCorrection("sp", "I", rid, gen, rulingFinding("the same thing", gen), false)
		}
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeEvicted)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 3 || st.Cause != CauseRepeatedFinding {
			t.Fatalf("a three-run of findings beside a two-run of checks reads %+v, want repeated_finding at count 3", st)
		}
	})

	t.Run("an acceptance clears the count", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		rid := f.startNode("sp", "I")
		f.addCorrection("sp", "I", rid, 2, rulingFinding("the same thing", 2), false)
		f.addCorrection("sp", "I", rid, 3, rulingFinding("the same thing", 3), false)
		if st := f.stagnationOf("sp", "I"); st == nil || st.Count != 2 {
			t.Fatalf("two corrections read %+v, want count 2", st)
		}
		// the corrected result is accepted at generation 3: the corrections before it are the history of a result that was repaired
		f.insertAcceptance(Acceptance{AcceptanceID: dig("acc-3"), PlanID: "sp", NodeID: "I", ManifestDigest: dig("manifest sp 3"), RelationshipID: rid, ExecutionGeneration: 3,
			EventID: "evt-rule-" + rid + "-3", RevisionHash: dig("revision acc 3"), CriteriaSetDigest: "criteria", Verdict: "verified", AckTier: "host_read",
			VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: "t", State: "active"})
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("an accepted result reads %+v, want the count cleared", st)
		}
	})

	t.Run("a head accepted again carries only its own failed checks", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		if st := f.stagnationOf("sp", "I"); st == nil || st.Count != 2 {
			t.Fatalf("two failed checks on the accepted head read %+v, want count 2", st)
		}
		// a REPAIRED head (another head) is accepted: the rows of the old head are that head’s history, and this one has failed nothing yet
		f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
		f.insertAcceptance(Acceptance{AcceptanceID: dig("acc-new"), PlanID: "sp", NodeID: "I", ManifestDigest: a.Manifest, RelationshipID: a.Acceptance.RelationshipID, ExecutionGeneration: 1,
			EventID: a.Event, RevisionHash: dig("revision acc new"), CriteriaSetDigest: "criteria", Verdict: "verified", HeadSHA: stagnatingHead2, Repository: "owner/repo", PRNumber: 7,
			AckTier: "host_read", VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: "t", State: "active"})
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("a repaired head accepted with no failed check reads %+v, want no object", st)
		}
	})

	t.Run("a revalidation does not raise a count of its own", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		// the accepted output is re-verified under the plan’s criteria: the same output, no new generation and no new child (E-11, E-21)
		f.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES ('rv1', ?, 'criteria', ?, 'verdict-turn', 1, 'parent', 't')",
			a.Acceptance.AcceptanceID, a.Event)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("a revalidated result reads %+v, want no object", st)
		}
		// a revalidation writes no correction generation, so it cannot be the identity of one either
		if n := f.count("SELECT COUNT(*) FROM dag_node_executions WHERE kind = 'correction'"); n != 0 {
			t.Fatalf("the revalidation wrote %d correction generations", n)
		}
	})

	t.Run("the reading prints the object and counts the rung", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		rid := f.startNode("sp", "I")
		before := f.read("sp")
		f.addCorrection("sp", "I", rid, 2, rulingFinding("the same thing", 2), false)
		f.addCorrection("sp", "I", rid, 3, rulingFinding("the same thing", 3), false)
		reading := f.read("sp")
		printed := asJSON(t, reading.Object())
		node := printed["nodes"].([]any)[0].(map[string]any)
		if node["node_id"] != "I" {
			t.Fatalf("the first node is %v", node["node_id"])
		}
		st, ok := node["stagnation"].(map[string]any)
		if !ok {
			t.Fatalf("the printed node carries no stagnation object: %v", node)
		}
		if st["count"] != float64(2) || st["cause"] != CauseRepeatedFinding || st["rung"] != RungEditPacket || st["next"] != RungSplitNode || st["finding_digest"] == nil || st["last_event_id"] == nil {
			t.Fatalf("printed stagnation = %v", st)
		}
		counts := printed["pass"].(map[string]any)["stagnation"].(map[string]any)
		if counts[RungEditPacket] != float64(1) || counts[RungFullReplan] != float64(0) {
			t.Fatalf("pass.stagnation = %v", counts)
		}
		if reading.InputDigest == before.InputDigest {
			t.Fatal("the input digest does not cover the stagnation object")
		}
		// a reading of a plan nobody has corrected twice prints the counts all zero and carries no object
		bare := f.read("sp").node("J")
		if bare.Stagnation != nil {
			t.Fatalf("a node that never stagnated reads %+v", bare.Stagnation)
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

// TestTheStagnationCounterReadsTheHeadItStandsOnAndCountsDistinctAttempts is the red test the issue body names: the five reproduced post-merge P1s on PR #710. The reading must count repeated
// required-check failures on the head the node's acceptance stands on now (standOf, so a recorded base refresh moves it), gathered from every acceptance of that head, by distinct failed attempt of
// the same required-name set with checks_pending rows transparent; and it must find the ruling that opened a correction generation at the nearest live generation below it, so a withdrawn generation
// neither counts nor breaks. Each subtest is one of the reproduced cases, and the two controls at the end pin the copies the merge lane writes.
func TestTheStagnationCounterReadsTheHeadItStandsOnAndCountsDistinctAttempts(t *testing.T) {
	t.Run("P1-1 failures on the head a base refresh moved the acceptance to are counted", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		// dag-base-refresh moves the acceptance onto H2; the acceptance row keeps H1, and the writer records H2 as head_sha (atStand). Only H2 failed, twice.
		f.recordStandHead(a, stagnatingHead2)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-h2-1", 1, stagnatingHead2, failuresJSON([]failure{{Name: "dev-gate", Run: "9", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-h2-2", 2, stagnatingHead2, failuresJSON([]failure{{Name: "dev-gate", Run: "9", Attempt: 2}}), 2, OutcomeEvicted)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedCheckFailure || st.Rung != RungEditPacket {
			t.Fatalf("two failures of the same required check on the refreshed head read %+v, want count 2 on the head the acceptance stands on", st)
		}
	})

	t.Run("P1-1 the head before a base refresh is that head's history", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		// the accepted head H1 failed twice before the base refresh; after the refresh the acceptance stands on H2, which has failed nothing, so those rows are H1's history
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-h1-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-h1-2", 2, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		f.recordStandHead(a, stagnatingHead2)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("the head before a base refresh reads %+v, want no object (another head's history)", st)
		}
	})

	t.Run("P1-2a the same failed attempt observed twice is one failure", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		// the same run and attempt of the required check, round 1 both times; only an optional check's result changed the evidence digest
		same := failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}})
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, same, 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, same, 1, OutcomeRetrySameSHA)
		if st := f.stagnationOf("sp", "I"); st != nil {
			t.Fatalf("one failed attempt observed twice reads %+v, want no object", st)
		}
	})

	t.Run("P1-2b pending re-runs between two failures do not end the run", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		// two re-runs in progress: a checks_pending row is neither counted nor a break, however many of them sit in the run
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, "[]", 1, OutcomeChecksPending)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-3", 3, head1, "[]", 1, OutcomeChecksPending)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-4", 4, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		// a pending row after the newest failure is transparent too
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-5", 5, head1, "[]", 2, OutcomeChecksPending)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedCheckFailure {
			t.Fatalf("fail, rerun pending, fail again reads %+v, want count 2", st)
		}
	})

	t.Run("P1-3 a withdrawn generation between two corrections does not hide the ruling", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		rid := a.Acceptance.RelationshipID
		f.addCorrection("sp", "I", rid, 2, rulingFinding("the same thing", 2), false)
		// generation 3 was opened by hand and withdrawn before it was sent: the relationship goes back to 2 and the number is never reused
		f.withdrawGeneration(rid, "sp", "I", 3, 2)
		// generation 2's report is ruled needs_changes with the same finding and opens generation 4
		f.addCorrectionFrom("sp", "I", rid, 4, 2, rulingFinding("the same thing", 4), false)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedFinding {
			t.Fatalf("corrections 2 and 4 with the same finding around a withdrawn 3 read %+v, want count 2", st)
		}
	})

	t.Run("P1-4 accepting the same head again keeps its unresolved repeated failure", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
		f.insertAcceptance(Acceptance{AcceptanceID: dig("acc-a2"), PlanID: "sp", NodeID: "I", ManifestDigest: a.Manifest, RelationshipID: a.Acceptance.RelationshipID, ExecutionGeneration: 1,
			EventID: a.Event, RevisionHash: dig("revision a2"), CriteriaSetDigest: "criteria", Verdict: "verified", HeadSHA: head1, Repository: "owner/repo", PRNumber: 7,
			AckTier: "host_read", VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: "t", State: "active"})
		// the merge judge copies the eviction of this head onto the acceptance in force as one row (mergejudge.go, round maxRounds)
		f.mergeCheckRow(dig("acc-a2"), "mc-a2-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), maxRounds, OutcomeEvicted)
		st := f.stagnationOf("sp", "I")
		// the copy is the same attempt as mc-2: it is not a third failure, and the two distinct attempts of the head are kept across both acceptances
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedCheckFailure {
			t.Fatalf("the same head accepted again after two failures reads %+v, want count 2 (the copied eviction is the same attempt)", st)
		}
	})

	t.Run("an attempt copied to a second acceptance of the same head is one failure", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		stagnatingPlan(f, "sp")
		a := f.acceptNode("sp", "I", pinnedOpts)
		// the first acceptance recorded both attempts of the head; the second (in force) carries only the copy of the newest, so the history of the head is still two distinct attempts
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 1}}), 1, OutcomeRetrySameSHA)
		f.mergeCheckRow(a.Acceptance.AcceptanceID, "mc-2", 2, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), 2, OutcomeEvicted)
		f.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", a.Acceptance.AcceptanceID)
		f.insertAcceptance(Acceptance{AcceptanceID: dig("acc-b2"), PlanID: "sp", NodeID: "I", ManifestDigest: a.Manifest, RelationshipID: a.Acceptance.RelationshipID, ExecutionGeneration: 1,
			EventID: a.Event, RevisionHash: dig("revision b2"), CriteriaSetDigest: "criteria", Verdict: "verified", HeadSHA: head1, Repository: "owner/repo", PRNumber: 7,
			AckTier: "host_read", VerdictTurnID: "verdict-turn", RuleVersionJSON: "{}", AcceptedByTask: "parent", AcceptedAt: "t", State: "active"})
		f.mergeCheckRow(dig("acc-b2"), "mc-b2-1", 1, head1, failuresJSON([]failure{{Name: "dev-gate", Run: "1", Attempt: 2}}), maxRounds, OutcomeEvicted)
		st := f.stagnationOf("sp", "I")
		if st == nil || st.Count != 2 || st.Cause != CauseRepeatedCheckFailure {
			t.Fatalf("an attempt copied to a second acceptance reads %+v, want count 2 across both acceptances of the head", st)
		}
	})
}
