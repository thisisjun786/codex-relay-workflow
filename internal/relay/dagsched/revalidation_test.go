package dagsched

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-284: what is done about a stale node (contract 3.2, 5, 8.4; E-11, E-19, E-20, E-21, E-25). The tests drive the real store, the real release path over the scripted host, the relay's own
// verdict writer for every ruling that opens a review or a generation, and the scheduler's own accept and correction functions. The printed words of the route are literals here, so a constant
// renamed in the code cannot silently rename what dag-ready prints.
const (
	rvRevalidate = "revalidate"
	rvCorrect    = "correct"
	rvHold       = "hold"
	rvRedefine   = "redefine"
)

// rvAction is the route the reading prints beside the stale object of a node ("" when the reading carries none).
func rvAction(t *testing.T, r Reading, node string) string {
	t.Helper()
	obj := invStaleObject(t, r, node)
	if obj == nil {
		t.Fatalf("%s carries no stale reading: %s", node, r.brief())
	}
	action, _ := invField(obj, "action").(string)
	return action
}

func rvDetail(t *testing.T, r Reading, node string) string {
	t.Helper()
	detail, _ := invField(invStaleObject(t, r, node), "action_detail").(string)
	return detail
}

// rvRows is the rows of a query, one line each, so that two states of the store can be compared as text.
func rvRows(k *releaseKit, query string, args ...any) string {
	k.t.Helper()
	rows, err := k.s.DB.Query(query, args...)
	if err != nil {
		k.t.Fatalf("%s: %v", query, err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []string
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			k.t.Fatal(err)
		}
		parts := make([]string, len(cells))
		for i, c := range cells {
			parts[i] = c.String
		}
		out = append(out, strings.Join(parts, "|"))
	}
	return strings.Join(out, "\n")
}

// rvRecords is what a node's valid result and the work paid for it leave in the store: its acceptances with their re-validations, its executions and its slot tenures.
func rvRecords(k *releaseKit, plan, node string) string {
	k.t.Helper()
	return strings.Join([]string{
		rvRows(k, "SELECT acceptance_id, state, manifest_digest, relationship_id, execution_generation, criteria_set_digest, COALESCE(supersedes_acceptance_id, '') FROM dag_acceptances WHERE plan_id = ? AND node_id = ? ORDER BY acceptance_id", plan, node),
		rvRows(k, "SELECT v.revalidation_id, v.criteria_set_digest, v.reval_seq FROM dag_acceptance_revalidations v JOIN dag_acceptances a ON a.acceptance_id = v.acceptance_id WHERE a.plan_id = ? AND a.node_id = ? ORDER BY v.revalidation_id", plan, node),
		rvRows(k, "SELECT relationship_id, execution_generation, manifest_digest, kind, COALESCE(managed_request_id, '') FROM dag_node_executions WHERE plan_id = ? AND node_id = ? ORDER BY execution_generation", plan, node),
		rvRows(k, "SELECT slot_id, state, tenure FROM execution_slots WHERE subject_kind = ? AND subject_key = ? ORDER BY tenure", SlotSubjectKind, SlotSubjectKey(plan, node)),
	}, "\n--\n")
}

// rvFleet is how many children the host was asked to create and how many relationships, generations, executions and releases the store holds.
func (k *releaseKit) rvFleet() string {
	k.t.Helper()
	created, sent := k.host.counts()
	return fmt.Sprintf("created=%d sent=%d relationships=%d children=%d generations=%d executions=%d releases=%d", created, sent, k.count("SELECT COUNT(*) FROM relationships"),
		k.count("SELECT COUNT(DISTINCT child_task_id) FROM relationships"), k.count("SELECT COUNT(*) FROM generations"), k.count("SELECT COUNT(*) FROM dag_node_executions"), k.count("SELECT COUNT(*) FROM dag_releases"))
}

// rvReregister is what the parent does when a node's criteria change: the plan revision names the new criteria digest, and the criteria registered for the node's relationship are replaced by the set
// with that digest (criteria-register), which is what opens a review of the head the relay had ruled under the old set.
func (k *releaseKit) rvReregister(plan, node, request, relationship, digest string, mutate func(n doc)) {
	k.t.Helper()
	k.invRevise(plan, node, request, func(n doc) {
		n["criteria_set_digest"] = digest
		if mutate != nil {
			mutate(n)
		}
	})
	k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", digest, relationship)
}

// rvRule is the parent's ruling through the relay's own verdict writer on the newest ready_for_review event of a relationship. A ruling on an event that is already ruled verified is accepted only
// as a re-review, which the relay opens when the criteria set changed since (the writer replays the same verdict and refuses a different one otherwise), so each call names the set the review read.
func (k *releaseKit) rvRule(relationship, verdict, expected string, findings ...delivery.Obj) {
	k.t.Helper()
	var child, event string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", relationship).Scan(&child); err != nil {
		k.t.Fatal(err)
	}
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ? AND outcome = 'ready_for_review' AND stage = 'final' ORDER BY execution_generation DESC, first_seen_at DESC, event_id DESC LIMIT 1", relationship).Scan(&event); err != nil {
		k.t.Fatal(err)
	}
	k.exec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", `["parent","`+child+`"]`, relationship)
	list := make([]any, len(findings))
	for i, f := range findings {
		list[i] = f
	}
	ack := delivery.NewAck(delivery.NewService(k.s, delivery.SystemClock{}))
	turn := fmt.Sprintf("verdict-turn-%s-%d", verdict, k.count("SELECT COUNT(*) FROM journal WHERE kind = 'verdict_recorded'"))
	record, err := ack.RecordVerdict(context.Background(), event, verdict, turn, nil, list, nil, expected)
	if err != nil {
		k.t.Fatalf("the relay's own verdict writer refused the %s ruling: %v", verdict, err)
	}
	for _, f := range record {
		if f.Key == "_replay" {
			k.t.Fatalf("the relay's verdict writer answered a replay of the ruling it already holds: no review was open (the set the node was ruled under is the registered one)")
		}
	}
}

func rvVerified() delivery.Obj {
	return delivery.Obj{{Key: "id", Value: "c1"}, {Key: "verdict", Value: "verified"}, {Key: "note", Value: "the same output meets the criteria as they stand now"}}
}

// rvPrepare is dag-correct --prepare for a node: the manifest rebuilt from the store as it is now and the instruction line the ruling carries.
func (k *releaseKit) rvPrepare(plan, node string) Prepared {
	k.t.Helper()
	return k.rvPrepareNotes(plan, node, "the notes of the rework")
}

// rvPrepareNotes is rvPrepare with the notes the correction snapshots: another text is another volatile input, so another manifest of the same node at the same slice.
func (k *releaseKit) rvPrepareNotes(plan, node, text string) Prepared {
	k.t.Helper()
	notes := writeFile(k.t, k.root, "rework-"+plan+"-"+node+"-"+shaOf([]byte(text))[:8]+".md", text)
	p, err := k.sched.PrepareCorrection(context.Background(), plan, node, "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: notes, SHA256: shaOf([]byte(text)), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: []string{k.root}})
	if err != nil {
		k.t.Fatalf("prepare %s: %v", node, err)
	}
	return p
}

// rvReportGeneration is the verified report a corrected generation leaves, as the writers shape it (the same rows seedReport leaves for the first one): the child's final report of that generation,
// its acknowledgement and evidence, and the parent's ruling under the criteria registered now.
func (k *releaseKit) rvReportGeneration(relationship, node string, generation int64, digest string) string {
	k.t.Helper()
	now := k.clock()
	var child string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", relationship).Scan(&child); err != nil {
		k.t.Fatal(err)
	}
	event := fmt.Sprintf("evt-g%d-%s", generation, relationship)
	var roots string
	if err := k.s.DB.QueryRow("SELECT artifact_roots FROM relationships WHERE relationship_id = ?", relationship).Scan(&roots); err != nil {
		k.t.Fatal(err)
	}
	var list []string
	if err := json.Unmarshal([]byte(roots), &list); err != nil || len(list) == 0 {
		k.t.Fatalf("the relationship has no artifact root: %v", err)
	}
	file := filepath.Join(list[0], fmt.Sprintf("%s-generation-%d.md", node, generation))
	content := []byte(fmt.Sprintf("the reworked artifact of %s, generation %d\n", node, generation))
	if err := os.WriteFile(file, content, 0o600); err != nil {
		k.t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	size := int64(len(content))
	revision, err := store.ManifestRevision([]store.ManifestEntry{{Path: file, SHA256: hex.EncodeToString(sum[:]), Bytes: &size}})
	if err != nil {
		k.t.Fatal(err)
	}
	receipt := fmt.Sprintf("{\"manifest\":[{\"path\":%s,\"sha256\":\"%s\",\"bytes\":%d}]}", jsonString(file), hex.EncodeToString(sum[:]), size)
	k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, ?, ?, 'ready_for_review', 'child', ?, ?, 'completed', ?, 'final', ?, ?)", event, relationship, generation, revision, child, fmt.Sprintf("turn-g%d", generation), receipt, now, now)
	k.exec("INSERT INTO revision_lineage (relationship_id, execution_generation, event_id, revision_hash, declared_by, recorded_at) VALUES (?, ?, ?, ?, 'child', ?)", relationship, generation, event, revision, now)
	k.exec("INSERT INTO acks (event_id, record, ack_turn_id, accepted, verified, ack_at) VALUES (?, '{}', 'ack-turn', 1, 'verified', ?)", event, now)
	k.exec("INSERT INTO ack_evidence (event_id, tier, observed_at) VALUES (?, 'host_read', ?)", event, now)
	k.exec("INSERT INTO verdicts (event_id, record, verdict, verdict_turn_id, decided_at) VALUES (?, '{}', 'verified', ?, ?)", event, fmt.Sprintf("verdict-turn-g%d", generation), now)
	k.exec("INSERT INTO verdict_context (event_id, set_digest, coverage, currency, head_event_id, head_revision, ack_evidence, recorded_at) VALUES (?, ?, '{}', 'current', ?, ?, '{}', ?)", event, digest, event, revision, now)
	return event
}

// rvSettledSharedRoot is the contract's shared-root case with every node released, reported and accepted: R -> A, R -> B, A -> C.
func rvSettledSharedRoot(t *testing.T) (*releaseKit, map[string]AcceptResult) {
	t.Helper()
	k := newReleaseKit(t)
	return k, invSharedRoot(k)
}

// Criterion c1 (contract 3.2, E-11, E-21): when only the criteria of a node changed, the consumed inputs and the output are what they were, so the same output is ruled again under the new criteria and the
// acceptance is re-verified: no new generation and no new child, the node below it stops being held back, and the nodes beside it keep their results and the records of the work behind them.
func TestCriteriaOnlyChangeRevalidatesTheSameOutputWithoutARerun(t *testing.T) {
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	other := dig("the second edition of A's criteria")
	siblings := map[string]string{"R": rvRecords(k, "sr", "R"), "B": rvRecords(k, "sr", "B")}
	fleet := k.rvFleet()

	k.rvReregister("sr", "A", "sr-r2", rid, other, nil)
	reading := k.read("sr")
	if got := invStaleIDs(reading); fmt.Sprint(got) != "[A C]" {
		t.Fatalf("stale = %v, want [A C]: %s", got, reading.brief())
	}
	if n := reading.node("A"); n.Reason != invCriteriaChanged {
		t.Fatalf("A = %+v", n)
	}
	if got := rvAction(t, reading, "A"); got != rvRevalidate {
		t.Fatalf("the route of A, whose criteria alone changed = %q, want %q", got, rvRevalidate)
	}
	if got := rvAction(t, reading, "C"); got != rvHold {
		t.Fatalf("the route of C, which rests on the stale A = %q, want %q (nothing to do on C until A is current)", got, rvHold)
	}
	if detail := rvDetail(t, reading, "C"); !strings.Contains(detail, "A") {
		t.Fatalf("the route of C does not name the predecessor it waits for: %q", detail)
	}

	// the review opens, the parent rules the same output verified under the new set, and the acceptance is re-verified
	k.rvRule(rid, "verified", other, rvVerified())
	res, err := k.accept("sr", "A", AcceptInput{})
	if err != nil || !res.Revalidated || res.Replayed || res.AcceptanceID != accepted["A"].AcceptanceID {
		t.Fatalf("accept after the review = %v %+v, want the same acceptance revalidated", err, res)
	}
	after := k.read("sr")
	if got := invStaleIDs(after); len(got) != 0 {
		t.Fatalf("after the revalidation %v are still stale: %s", got, after.brief())
	}
	if now := k.rvFleet(); now != fleet {
		t.Fatalf("the revalidation opened a generation or made a child:\nbefore %s\nafter  %s", fleet, now)
	}
	if k.count("SELECT COUNT(*) FROM dag_acceptances") != 4 || k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations") != 1 {
		t.Fatalf("%d acceptances and %d revalidations", k.count("SELECT COUNT(*) FROM dag_acceptances"), k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations"))
	}
	for node, before := range siblings {
		if now := rvRecords(k, "sr", node); now != before {
			t.Fatalf("the records of the unrelated node %s changed:\nbefore %s\nafter  %s", node, before, now)
		}
		if n := after.node(node); n.Reason != DoneAccepted {
			t.Fatalf("%s = %+v", node, n)
		}
	}
}

// A re-verification resolves a change of the criteria and nothing else (contract 3.2: the same output is judged again only when the output is what it was). A node that is stale for another reason is
// not made current by ruling its old output again, so the call is refused before it writes a re-validation that would only look like progress.
func TestRevalidationIsRefusedWhenTheOutputMustBeReworked(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(k *releaseKit, accepted map[string]AcceptResult) string // returns the digest of the new criteria
		reason string                                                       // the stale reason before the call
		action string                                                       // the route the reading prints for A
		names  string                                                       // what the refusal names
	}{
		{name: "the node's own spec changed together with its criteria", reason: invSliceChanged, action: rvCorrect, names: "dag-correct", setup: func(k *releaseKit, accepted map[string]AcceptResult) string {
			other := dig("criteria with a changed spec")
			k.rvReregister("sr", "A", "sr-r2", accepted["A"].RelationshipID, other, func(n doc) { n["title"] = "a spec that changed with it" })
			return other
		}},
		{name: "the criteria changed and so did something A consumed", reason: invCriteriaChanged, action: rvCorrect, names: "dag-correct", setup: func(k *releaseKit, accepted map[string]AcceptResult) string {
			// R is revised and accepted again at its new slice: the acceptance of R that A consumed is no longer the active one
			k.invRevise("sr", "R", "sr-r2", invTitle("R revised"))
			again := k.invSettleAgain("sr", "R", accepted["R"])
			if again.AcceptanceID == accepted["R"].AcceptanceID {
				k.t.Fatal("R was not accepted again")
			}
			other := dig("criteria after R changed")
			k.rvReregister("sr", "A", "sr-r3", accepted["A"].RelationshipID, other, nil)
			return other
		}},
		{name: "the criteria changed and an input A consumed is no longer there", reason: invCriteriaChanged, action: rvHold, names: "is not there", setup: func(k *releaseKit, accepted map[string]AcceptResult) string {
			// the child of R was cancelled: the acceptance A consumed stands, but the edge from R is no longer satisfied, so what A consumed cannot be rebuilt
			k.exec("UPDATE relationships SET status = 'cancelled' WHERE relationship_id = ?", accepted["R"].RelationshipID)
			other := dig("criteria while R is gone")
			k.rvReregister("sr", "A", "sr-r2", accepted["A"].RelationshipID, other, nil)
			return other
		}},
		{name: "the criteria changed while R, which A rests on, is stale", reason: invCriteriaChanged, action: rvHold, names: "nothing to do on A yet", setup: func(k *releaseKit, accepted map[string]AcceptResult) string {
			k.invRevise("sr", "R", "sr-r2", invTitle("R revised"))
			other := dig("criteria while R is stale")
			k.rvReregister("sr", "A", "sr-r3", accepted["A"].RelationshipID, other, nil)
			return other
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, accepted := rvSettledSharedRoot(t)
			other := c.setup(k, accepted)
			reading := k.read("sr")
			if n := reading.node("A"); n.Reason != c.reason {
				t.Fatalf("A = %+v, want %s: %s", n, c.reason, reading.brief())
			}
			if got := rvAction(t, reading, "A"); got != c.action {
				t.Fatalf("the route of A = %q, want %q: the output cannot be ruled again as it stands", got, c.action)
			}
			k.rvRule(accepted["A"].RelationshipID, "verified", other, rvVerified())
			before := rvRecords(k, "sr", "A")
			if _, err := k.accept("sr", "A", AcceptInput{}); refusalReason(err) != "disposition_conflict" {
				t.Fatalf("accept of the same output = %v, want a disposition_conflict that names the rework", err)
			} else if !strings.Contains(err.Error(), c.names) {
				t.Fatalf("the refusal does not name the way on (%q): %v", c.names, err)
			}
			if k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations") != 0 || rvRecords(k, "sr", "A") != before {
				t.Fatalf("a refused revalidation wrote rows:\n%s", rvRecords(k, "sr", "A"))
			}
			if n := k.read("sr").node("A"); n.Disposition != DispStale {
				t.Fatalf("A = %+v, want it still stale", n)
			}
		})
	}
}

// invSettleAgain accepts a node's result again after a revision changed what it consumed: the acceptance of the first one is superseded and a second acceptance is written for the same relationship's
// next report, consuming what the product builds now (invRealInputs), as the repair of a seed leaves it.
func (k *releaseKit) invSettleAgain(plan, node string, first AcceptResult) Acceptance {
	k.t.Helper()
	inputs := invRealInputs(k, plan, node)
	k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", first.AcceptanceID)
	return k.acceptNode(plan, node, acceptOpts{Suffix: "-2", Inputs: inputs}).Acceptance
}

// Criterion c2 (contract 3.2, 5 ID-3, E-19, E-25): when the output has to be reworked the correction goes back to the same child as the next generation of the same relationship, through dag-correct: the
// manifest is rebuilt for the node as the plan holds it now, the relay's own verdict writer opens the generation and sends the instruction, and recording it binds that manifest. No child is made again,
// the unrelated nodes keep their results and records, and when the reworked output is accepted what rested on the old one is held back until it is accepted again itself.
func TestOutputReworkGoesToTheSameChildAsANewGeneration(t *testing.T) {
	k, accepted := rvSettledSharedRoot(t)
	rid := accepted["A"].RelationshipID
	var childBefore string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", rid).Scan(&childBefore); err != nil {
		t.Fatal(err)
	}
	siblings := map[string]string{"R": rvRecords(k, "sr", "R"), "B": rvRecords(k, "sr", "B")}
	fleet := k.rvFleet()

	// the spec of A changes together with its criteria: the output cannot be ruled again as it is
	other := dig("criteria of the reworked A")
	k.rvReregister("sr", "A", "sr-r2", rid, other, func(n doc) { n["title"] = "A, as the revision specifies it" })
	reading := k.read("sr")
	if n := reading.node("A"); n.Reason != invSliceChanged {
		t.Fatalf("A = %+v: %s", n, reading.brief())
	}
	if got := rvAction(t, reading, "A"); got != rvCorrect {
		t.Fatalf("the route of A = %q, want %q", got, rvCorrect)
	}
	if detail := rvDetail(t, reading, "A"); !strings.Contains(detail, "dag-correct") || !strings.Contains(detail, "same child") {
		t.Fatalf("the route of A does not say where the rework goes: %q", detail)
	}

	prepared := k.rvPrepare("sr", "A")
	k.rvRule(rid, "needs_changes", other, restoration(prepared.Instruction))
	if k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ?", rid) != 2 {
		t.Fatal("the ruling did not open the next generation of the relationship")
	}
	// the reading says the correction is under way, not that another one is due
	if got := rvAction(t, k.read("sr"), "A"); got != rvHold {
		t.Fatalf("the route of A while generation 2 is open = %q, want %q", got, rvHold)
	} else if detail := rvDetail(t, k.read("sr"), "A"); !strings.Contains(detail, "generation 2") {
		t.Fatalf("the route of A does not name the open generation: %q", detail)
	}
	res, err := k.sched.RecordCorrection(context.Background(), "sr", "A", "parent", "")
	if err != nil || res.Replayed || res.CarriedOver || res.Generation != 2 || res.RelationshipID != rid || res.ManifestDigest != prepared.ManifestDigest {
		t.Fatalf("dag-correct = %v %+v", err, res)
	}
	var childAfter string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", rid).Scan(&childAfter); err != nil || childAfter != childBefore {
		t.Fatalf("the correction went to %q, the child of the node is %q (%v)", childAfter, childBefore, err)
	}
	if got := k.rvFleet(); !strings.HasPrefix(got, strings.Split(fleet, " generations=")[0]) || k.count("SELECT COUNT(*) FROM relationships") != 4 {
		t.Fatalf("the correction recreated a child:\nbefore %s\nafter  %s", fleet, got)
	}
	if k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'A'") != 2 || k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'A' AND kind = 'correction' AND execution_generation = 2") != 1 {
		t.Fatal("generation 2 is not recorded as the correction execution of A")
	}

	// the child reports the reworked output and the parent accepts it in place of the first one
	k.rvReportGeneration(rid, "A", 2, other)
	second, err := k.accept("sr", "A", AcceptInput{Supersedes: accepted["A"].AcceptanceID})
	if err != nil || second.Revalidated || second.Replayed || second.SupersededID != accepted["A"].AcceptanceID || second.AcceptanceID == accepted["A"].AcceptanceID || second.Generation != 2 {
		t.Fatalf("accept of the reworked output = %v %+v", err, second)
	}
	after := k.read("sr")
	if got := invStaleIDs(after); fmt.Sprint(got) != "[C]" {
		t.Fatalf("after A was accepted again stale = %v, want [C]: %s", got, after.brief())
	}
	if n := after.node("A"); n.Reason != DoneAccepted {
		t.Fatalf("A = %+v", n)
	}
	obj := invStaleObject(t, after, "C")
	if invField(obj, "cause") != "input_changed" || invField(obj, "consumed_acceptance_id") != accepted["A"].AcceptanceID || invField(obj, "current_acceptance_id") != second.AcceptanceID {
		t.Fatalf("C = %v", obj)
	}
	if got := rvAction(t, after, "C"); got != rvCorrect {
		t.Fatalf("the route of C, which consumed the first acceptance of A = %q, want %q", got, rvCorrect)
	}
	// C was ruled under criteria that did not change and is accepted, so the relay's verdict writer refuses another ruling (disposition_conflict) and no generation could open: the reading says so, and the way the relay
	// opens a review (the criteria registered for C change, which the plan revision that names the new digest does) is the one that goes through
	if detail := rvDetail(t, after, "C"); !strings.Contains(detail, "disposition_conflict") {
		t.Fatalf("the route of C does not say that no ruling can open its generation yet: %q", detail)
	}
	ridC := accepted["C"].RelationshipID
	var childC string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", ridC).Scan(&childC); err != nil {
		t.Fatal(err)
	}
	otherC := dig("criteria of the reworked C")
	k.rvReregister("sr", "C", "sr-r3", ridC, otherC, nil)
	reviewing := k.read("sr")
	if got := rvAction(t, reviewing, "C"); got != rvCorrect {
		t.Fatalf("the route of C after its criteria changed = %q, want %q", got, rvCorrect)
	}
	if detail := rvDetail(t, reviewing, "C"); !strings.Contains(detail, "is open") {
		t.Fatalf("the route of C does not say that the review is open: %q", detail)
	}
	preparedC := k.rvPrepare("sr", "C")
	k.rvRule(ridC, "needs_changes", otherC, restoration(preparedC.Instruction))
	resC, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", "")
	if err != nil || resC.Replayed || resC.CarriedOver || resC.Generation != 2 || resC.RelationshipID != ridC || resC.ManifestDigest != preparedC.ManifestDigest {
		t.Fatalf("dag-correct for C = %v %+v", err, resC)
	}
	var childCAfter string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", ridC).Scan(&childCAfter); err != nil || childCAfter != childC {
		t.Fatalf("the correction of C went to %q, the child of C is %q (%v)", childCAfter, childC, err)
	}
	k.rvReportGeneration(ridC, "C", 2, otherC)
	secondC, err := k.accept("sr", "C", AcceptInput{Supersedes: accepted["C"].AcceptanceID})
	if err != nil || secondC.SupersededID != accepted["C"].AcceptanceID || secondC.Generation != 2 {
		t.Fatalf("accept of the reworked C = %v %+v", err, secondC)
	}
	after = k.read("sr")
	if got := invStaleIDs(after); len(got) != 0 {
		t.Fatalf("after A and C were reworked %v are still stale: %s", got, after.brief())
	}
	if got := k.rvFleet(); !strings.HasPrefix(got, strings.Split(fleet, " generations=")[0]) {
		t.Fatalf("the rework recreated a child:\nbefore %s\nafter  %s", fleet, got)
	}
	for node, before := range siblings {
		if now := rvRecords(k, "sr", node); now != before {
			t.Fatalf("the records of the unrelated node %s changed:\nbefore %s\nafter  %s", node, before, now)
		}
		if n := after.node(node); n.Reason != DoneAccepted {
			t.Fatalf("%s = %+v", node, n)
		}
	}
}

// rvMerged is the plan U -> I -> K over a real repository with U and K settled and I, an implementation node, accepted and landed on dev: it reads integrated, and K, which rests on its landing, is accepted.
func rvMerged(t *testing.T) (*integrationKit, accepted) {
	t.Helper()
	k := newIntegrationKit(t)
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("U", dag.NodeNonPR), addEdge("ui", "U", "I", dag.EdgeArtifactVerified, nil))
	k.invSettle("g", "U")
	repo.git("checkout", "-q", "-b", "feature")
	feature := repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	a := k.acceptNode("g", "I", acceptOpts{HeadSHA: feature, PR: 5, Forge: "owner/repo", Repository: repo.path, Inputs: invRealInputs(k.releaseKit, "g", "I")})
	k.holdSlotsFor("g", "I")
	repo.git("merge", "-q", "--no-ff", "-m", "merge feature", "feature")
	k.mark(a)
	if res, err := k.observe(); err != nil || !res.Integrated {
		t.Fatalf("I did not integrate: %v %+v", err, res)
	}
	k.invSettle("g", "K")
	return k, a
}

// Criterion c3 (contract 5, 8.4 E-20): a node that already merged is never run again when something above it changes. Its result is in the target branch, so it reads integrated and not stale, the
// scheduler gives it no new child, no correction is prepared for it and none is recorded as its own, and nothing that rests on its landing moves. The change is carried by new nodes of a new plan
// revision: a successor of the merged node is released like any other node, one that rests on the changed node waits until that node is current, and the merged node's records stay as they were.
func TestMergedNodeIsNeverRerunWhenItsUpstreamChanges(t *testing.T) {
	k, a := rvMerged(t)
	repo := k.repo
	merged, consumer := rvRecords(k.releaseKit, "g", "I"), rvRecords(k.releaseKit, "g", "K")
	fleet := k.rvFleet()
	generations := k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ?", a.Acceptance.RelationshipID)

	// what is above the merged node, and the merged node's own spec, change
	k.invRevise("g", "U", "g-r3", invTitle("U, as the new revision specifies it"))
	k.invRevise("g", "I", "g-r4", invTitle("I, as the new revision specifies it"))
	reading := k.read("g")
	if got := invStaleIDs(reading); fmt.Sprint(got) != "[U]" {
		t.Fatalf("stale = %v, want [U]: a node that merged was marked (%s)", got, reading.brief())
	}
	if n := reading.node("I"); n.State != StateIntegrated || n.Reason != DoneIntegrated {
		t.Fatalf("I = %+v", n)
	}
	if got := rvAction(t, reading, "U"); got != rvCorrect {
		t.Fatalf("the route of U = %q, want %q", got, rvCorrect)
	}

	unchanged := func(what string) {
		t.Helper()
		if now := rvRecords(k.releaseKit, "g", "I"); now != merged {
			t.Fatalf("after %s the records of the merged node changed:\nbefore %s\nafter  %s", what, merged, now)
		}
		if now := rvRecords(k.releaseKit, "g", "K"); now != consumer {
			t.Fatalf("after %s the records of what rests on its landing changed:\nbefore %s\nafter  %s", what, consumer, now)
		}
		if now := k.rvFleet(); now != fleet {
			t.Fatalf("after %s the fleet changed:\nbefore %s\nafter  %s", what, fleet, now)
		}
	}
	// the scheduler does not release it again
	if res, err := k.release("g", "I"); err == nil && !res.Replayed {
		t.Fatalf("release of the merged node = %+v, want no new child", res)
	}
	unchanged("a release of the merged node")
	// no correction is prepared for it, and a generation the relay opened for it is not recorded as an execution of the node
	if _, err := k.sched.PrepareCorrection(context.Background(), "g", "I", "parent", ManifestInput{RuleVersion: k.request(true).RuleVersion,
		Base: &BaseRef{Repository: repo.path, Ref: "dev", SHA: repo.git("rev-parse", "dev")}}, VerifyOptions{ArtifactRoots: []string{k.root}}); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("prepare a correction of the merged node = %v, want disposition_conflict", err)
	} else if !strings.Contains(err.Error(), "successor") {
		t.Fatalf("the refusal does not name the way on: %v", err)
	}
	unchanged("a prepared correction")

	// the change is carried by successors in a new plan revision
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r5", addRelNode("N2", dag.NodeNonPR), addRelNode("N3", dag.NodeNonPR),
		addEdge("in2", "I", "N2", dag.EdgeIntegrated, doc{"target_repository": repo.path}), addEdge("un3", "U", "N3", dag.EdgeArtifactVerified, nil))
	reading = k.read("g")
	if n := reading.node("N2"); n.Disposition != DispReady {
		t.Fatalf("N2, a successor of the merged node = %+v, want ready", n)
	}
	if n := reading.node("N3"); n.Reason != BlockedStalePredecessor {
		t.Fatalf("N3, built on the changed U = %+v, want %s", n, BlockedStalePredecessor)
	}
	created, _ := k.host.counts()
	k.mustRelease("g", "N2")
	if again, _ := k.host.counts(); again != created+1 {
		t.Fatalf("releasing the successor made %d children, want 1", again-created)
	}
	if now := rvRecords(k.releaseKit, "g", "I"); now != merged {
		t.Fatalf("the records of the merged node changed when its successor was released:\nbefore %s\nafter  %s", merged, now)
	}
	if n := k.read("g").node("I"); n.State != StateIntegrated {
		t.Fatalf("I = %+v", n)
	}

	// the relay's own verdict writer opens the next generation for the merged node when its review is open (the criteria registered for it changed): that is the relay's act, and the scheduler refuses to
	// record the generation as an execution of the node, so nothing the scheduler binds or accepts is a rerun of it
	other := dig("the second edition of I's criteria")
	k.rvReregister("g", "I", "g-r6", a.Acceptance.RelationshipID, other, nil)
	k.rvRule(a.Acceptance.RelationshipID, "needs_changes", other, restoration("do it again"))
	if k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ?", a.Acceptance.RelationshipID) != generations+1 {
		t.Fatal("the relay's verdict writer did not open the generation the scheduler is to refuse")
	}
	if _, err := k.sched.RecordCorrection(context.Background(), "g", "I", "parent", ""); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("record a correction of the merged node = %v, want disposition_conflict", err)
	} else if !strings.Contains(err.Error(), "successor") {
		t.Fatalf("the refusal does not name the way on: %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'I'") != 1 {
		t.Fatal("a generation was recorded as a rerun of the merged node")
	}
}

// Contract 3.2, 5 and D-08: the route follows the cause. A change that leaves the output as it was needs it ruled again; one that changes what the output rests on sends it back to the same child;
// one that rests on a stale predecessor waits for it; and a node whose relationship has ended has no child to correct, so a rework is a redefinition (a new relationship that supersedes) and is
// the parent's decision.
func TestStaleRouteFollowsTheCause(t *testing.T) {
	type route struct{ node, action string }
	cases := []struct {
		name  string
		setup func(k *releaseKit, accepted map[string]AcceptResult)
		want  []route
	}{
		{name: "the spec of A changed", setup: func(k *releaseKit, _ map[string]AcceptResult) { k.invRevise("sr", "A", "sr-r2", invTitle("A revised")) },
			want: []route{{"A", rvCorrect}, {"C", rvHold}}},
		{name: "an edge was added into C", setup: func(k *releaseKit, _ map[string]AcceptResult) {
			k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", addEdge("bc", "B", "C", dag.EdgeArtifactVerified, nil))
		}, want: []route{{"C", rvCorrect}}},
		{name: "an edge was added into C from a node nobody accepted", setup: func(k *releaseKit, _ map[string]AcceptResult) {
			k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", addRelNode("Q", dag.NodeNonPR), addEdge("qc", "Q", "C", dag.EdgeArtifactVerified, nil))
		}, want: []route{{"C", rvHold}}},
		{name: "the plan paused A, whose spec changed", setup: func(k *releaseKit, _ map[string]AcceptResult) {
			k.invRevise("sr", "A", "sr-r2", invTitle("A revised"))
			k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r3", lifeOp("pause_node", "A"))
		}, want: []route{{"A", rvHold}, {"C", rvHold}}},
		{name: "an edge was retired from C", setup: func(k *releaseKit, _ map[string]AcceptResult) {
			k.putPlan("sr", int(k.snapshot("sr").Revision), "sr-r2", doc{"op": dag.OpRetireEdge, "edge_id": "ac"})
		}, want: []route{{"C", rvCorrect}}},
		{name: "A was accepted again after its spec changed", setup: func(k *releaseKit, accepted map[string]AcceptResult) {
			k.invRevise("sr", "A", "sr-r2", invTitle("A revised"))
			k.invSettleAgain("sr", "A", accepted["A"])
		}, want: []route{{"C", rvCorrect}}},
		{name: "the criteria of A changed", setup: func(k *releaseKit, accepted map[string]AcceptResult) {
			k.rvReregister("sr", "A", "sr-r2", accepted["A"].RelationshipID, dig("A's new criteria"), nil)
		}, want: []route{{"A", rvRevalidate}, {"C", rvHold}}},
		{name: "the criteria of A changed and the child of A is gone", setup: func(k *releaseKit, accepted map[string]AcceptResult) {
			k.rvReregister("sr", "A", "sr-r2", accepted["A"].RelationshipID, dig("A's new criteria"), nil)
			k.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = ?", accepted["A"].RelationshipID)
		}, want: []route{{"A", rvRedefine}, {"C", rvHold}}},
		{name: "the criteria of A changed while R, which A rests on, is stale", setup: func(k *releaseKit, accepted map[string]AcceptResult) {
			k.invRevise("sr", "R", "sr-r2", invTitle("R revised"))
			k.rvReregister("sr", "A", "sr-r3", accepted["A"].RelationshipID, dig("A's new criteria"), nil)
		}, want: []route{{"R", rvCorrect}, {"A", rvHold}, {"B", rvHold}, {"C", rvHold}}},
		{name: "R was accepted again and is stale again while the criteria of A changed", setup: func(k *releaseKit, accepted map[string]AcceptResult) {
			k.invRevise("sr", "R", "sr-r2", invTitle("R revised"))
			k.invSettleAgain("sr", "R", accepted["R"])
			k.invRevise("sr", "R", "sr-r3", invTitle("R revised again"))
			k.rvReregister("sr", "A", "sr-r4", accepted["A"].RelationshipID, dig("A's new criteria"), nil)
		}, want: []route{{"R", rvCorrect}, {"A", rvHold}, {"B", rvHold}, {"C", rvHold}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k, accepted := rvSettledSharedRoot(t)
			c.setup(k, accepted)
			reading := k.read("sr")
			stale := invStaleIDs(reading)
			if len(stale) != len(c.want) {
				t.Fatalf("stale = %v, want the nodes of %v: %s", stale, c.want, reading.brief())
			}
			for _, w := range c.want {
				if got := rvAction(t, reading, w.node); got != w.action {
					t.Errorf("the route of %s = %q, want %q: %s", w.node, got, w.action, reading.brief())
				}
				if detail := rvDetail(t, reading, w.node); detail == "" {
					t.Errorf("the route of %s says nothing of why", w.node)
				}
			}
			// a node that is not stale prints no route
			for _, n := range reading.Nodes {
				if n.Disposition != DispStale && n.Stale != nil {
					t.Errorf("%s is not stale and carries a stale reading", n.NodeID)
				}
			}
		})
	}
	t.Run("a relationship that has ended is not corrected, and the reason says so", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		k.invRevise("sr", "A", "sr-r2", invTitle("A revised"))
		k.exec("UPDATE relationships SET status = 'archived' WHERE relationship_id = ?", accepted["A"].RelationshipID)
		if got := rvAction(t, k.read("sr"), "A"); got != rvRedefine {
			t.Fatalf("the route of A = %q, want %q", got, rvRedefine)
		}
		if detail := rvDetail(t, k.read("sr"), "A"); !strings.Contains(detail, "supersedes") {
			t.Fatalf("the route does not name the redefinition: %q", detail)
		}
		if _, err := k.sched.PrepareCorrection(context.Background(), "sr", "A", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{ArtifactRoots: []string{k.root}}); refusalReason(err) != "relationship_not_active" {
			t.Fatalf("prepare a correction for an ended relationship = %v", err)
		}
	})
}

// The page that describes the reading names every route and the words the code prints for it.
func TestSchedulerPageNamesEveryStaleRoute(t *testing.T) {
	raw, err := os.ReadFile("../../../docs/relay/dag-scheduler.md")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, route := range []string{rvRevalidate, rvCorrect, rvHold, rvRedefine} {
		if !strings.Contains(page, "`"+route+"`") {
			t.Errorf("docs/relay/dag-scheduler.md does not name the route %s", route)
		}
	}
}

// A landing does not make a result current (docs/relay/dag-scheduler.md, Edge satisfaction): when the criteria of a node that landed change, the node is not stale and is not run again, what it
// hands over waits, and the way on is the one a node that is not stale has: the same output is ruled again under the new criteria and accepted again, which records a revalidation, makes no
// generation and no child, and is not refused as the revalidation of a stale node is.
func TestAMergedNodeIsRevalidatedNotRerunAfterACriteriaChange(t *testing.T) {
	k, a := rvMerged(t)
	rid := a.Acceptance.RelationshipID
	fleet := k.rvFleet()
	other := dig("the second edition of I's criteria")
	k.rvReregister("g", "I", "g-r3", rid, other, nil)
	reading := k.read("g")
	if n := reading.node("I"); n.State != StateIntegrated || n.Reason != DoneIntegrated {
		t.Fatalf("I = %+v: a node that landed is not stale", n)
	}
	if got := invStaleIDs(reading); len(got) != 0 {
		t.Fatalf("stale = %v: %s", got, reading.brief())
	}
	if st := k.status("g", "ik"); st.Satisfied || st.Reason != BlockedStaleCriteria {
		t.Fatalf("the edge from the merged node = %+v, want %s until the same output is ruled under the new criteria", st, BlockedStaleCriteria)
	}
	// the review opens and the parent rules the same output verified under the new set
	k.forge.by["owner/repo#5"] = PullRequest{Repository: "owner/repo", Number: 5, State: "merged", HeadSHA: a.Acceptance.HeadSHA, BaseRef: "dev", BaseSHA: head1, Verdict: "ready",
		RequiredDeclared: []string{"test"}, RequiredReadable: true, Checks: []Check{{RunID: "1", Name: "test", HeadSHA: a.Acceptance.HeadSHA, Conclusion: "success", Attempt: 1}}}
	k.rvRule(rid, "verified", other, rvVerified())
	res, err := k.accept("g", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 5}})
	if err != nil || !res.Revalidated || res.AcceptanceID != a.Acceptance.AcceptanceID {
		t.Fatalf("accept of the merged node's output after the criteria changed = %v %+v, want a revalidation of the same acceptance", err, res)
	}
	if st := k.status("g", "ik"); !st.Satisfied {
		t.Fatalf("after the revalidation the edge from the merged node = %+v", st)
	}
	if now := k.rvFleet(); now != fleet {
		t.Fatalf("the revalidation of a merged node opened a generation or made a child:\nbefore %s\nafter  %s", fleet, now)
	}
}

// What landed is judged from the acceptance, not from what the plan now says the node is (contract 8.4, E-20): a revision that changes the kind of a node whose pull request merged does not make it
// correctable, so no correction is prepared for it and it is not run again.
func TestAMergedNodeIsNotCorrectableWhateverItsKindBecomes(t *testing.T) {
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature-d")
	head := repo.commit("d.txt", "d")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "D", "d.txt")
	a := k.acceptNode("g", "D", acceptOpts{HeadSHA: head, PR: 6, Forge: "owner/repo", Repository: repo.path})
	k.holdSlotsFor("g", "D")
	repo.git("merge", "-q", "--no-ff", "-m", "merge d", "feature-d")
	k.mark(a)
	if res, err := k.sched.ObserveIntegration(context.Background(), "g", "D", "parent", []Target{{Repository: repo.path, BaseRef: "dev"}}); err != nil || !res.Integrated {
		t.Fatalf("D did not integrate: %v %+v", err, res)
	}
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", doc{"op": dag.OpUpdateNode, "node": relNode("D", dag.NodeNonPR)})
	if n, _ := nodeOf(k.snapshot("g"), "D"); n.Kind != dag.NodeNonPR {
		t.Fatalf("the revision did not change the kind of D: %+v", n)
	}
	reading := k.read("g")
	if got := invStaleIDs(reading); len(got) != 0 {
		t.Fatalf("stale = %v: a node that landed is not stale whatever kind the plan gives it", got)
	}
	if n := reading.node("D"); n.State != StateIntegrated || n.Reason != DoneIntegrated {
		t.Fatalf("D = %+v: what landed still reads integrated whatever kind the plan gives the node", n)
	}
	if _, err := k.sched.PrepareCorrection(context.Background(), "g", "D", "parent", ManifestInput{RuleVersion: k.request(false).RuleVersion}, VerifyOptions{ArtifactRoots: []string{k.root}}); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("prepare a correction of a node that landed and is now another kind = %v, want disposition_conflict", err)
	} else if !strings.Contains(err.Error(), "landed") {
		t.Fatalf("the refusal does not say why: %v", err)
	}
}

// rvHandKit is a stale node whose criteria did not change: A is revised and accepted again, so C, which consumed the first acceptance of A, rests on a replaced input and nothing else. The relay's
// verdict writer holds C's head as ruled verified under the criteria registered now.
func rvHandKit(t *testing.T) (*releaseKit, map[string]AcceptResult, string, Prepared) {
	t.Helper()
	k, accepted := rvSettledSharedRoot(t)
	k.invRevise("sr", "A", "sr-r2", invTitle("A revised"))
	k.invSettleAgain("sr", "A", accepted["A"])
	reading := k.read("sr")
	if n := reading.node("C"); n.Disposition != DispStale {
		t.Fatalf("C = %+v, want it stale: %s", n, reading.brief())
	}
	return k, accepted, accepted["C"].RelationshipID, k.rvPrepare("sr", "C")
}

// rvOpenByHand is the coordinator's act with the relay's own commands: generation-open under the request id dag-correct --prepare printed, then generation-bind to the turn that carried the instruction.
func rvOpenByHand(t *testing.T, k *releaseKit, relationship, request string, bind bool) {
	t.Helper()
	reg := &registry.Registry{Store: k.s}
	if _, err := reg.OpenGeneration(context.Background(), relationship, request, "needs_changes_revision", sql.NullString{}); err != nil {
		t.Fatalf("generation-open: %v", err)
	}
	if bind {
		if _, err := reg.BindAnchor(context.Background(), relationship, 2, "turn-dispatch-"+relationship, "dispatch_receipt"); err != nil {
			t.Fatalf("generation-bind: %v", err)
		}
	}
}

// Criterion c2, generation 2 of the packet: a node made stale because an input it consumed was replaced, its criteria untouched, is corrected by the same child through a generation the coordinator opens by hand
// (the relay's generation-open and generation-bind). The relay's own writer refuses a ruling on that accepted head with disposition_conflict and opens no generation, which the test shows first; then dag-correct binds the hand-opened
// generation to the manifest it was prepared for, records how the instruction reached the child, and the reworked output is accepted with --supersedes. No relationship and no child is made.
func TestReworkWithUnchangedCriteriaGoesThroughAGenerationOpenedByHand(t *testing.T) {
	k, accepted, ridC, prepared := rvHandKit(t)
	var childBefore string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", ridC).Scan(&childBefore); err != nil {
		t.Fatal(err)
	}
	siblings := map[string]string{"R": rvRecords(k, "sr", "R"), "B": rvRecords(k, "sr", "B"), "A": rvRecords(k, "sr", "A")}
	fleet := k.rvFleet()
	prefix := strings.Split(fleet, " generations=")[0]

	reading := k.read("sr")
	obj := invStaleObject(t, reading, "C")
	if invField(obj, "cause") != "input_changed" || rvAction(t, reading, "C") != rvCorrect {
		t.Fatalf("C = %v, route %q", obj, rvAction(t, reading, "C"))
	}
	if detail := rvDetail(t, reading, "C"); !strings.Contains(detail, "generation-open") || !strings.Contains(detail, "disposition_conflict") || !strings.Contains(detail, "generation-bind") {
		t.Fatalf("the route of C does not name the generation opened by hand: %q", detail)
	}

	// the relay's own verdict writer takes no second ruling on that accepted head: a refusal that names the plan's acceptance and the route, no generation
	var child string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", ridC).Scan(&child); err != nil {
		t.Fatal(err)
	}
	k.exec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", `["parent","`+child+`"]`, ridC)
	record, err := delivery.NewAck(delivery.NewService(k.s, delivery.SystemClock{})).RecordVerdict(context.Background(), "evt-"+ridC, "needs_changes", "verdict-turn-probe", nil, []any{restoration("do it again")}, nil, releaseCriteriaDigest())
	if refusalReason(err) != "disposition_conflict" || record != nil || !strings.Contains(err.Error(), "accepted it as acceptance") || k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ?", ridC) != 1 {
		t.Fatalf("the relay's writer on C's accepted head = %v %v: the premise of the hand-opened route does not hold", record, err)
	}

	// the coordinator opens the generation under the id the prepare printed, sends the instruction, binds the turn
	if prepared.DispatchRequestID == "" || prepared.DispatchRequestID != CorrectionRequestID("sr", "C", prepared.ManifestDigest, 2) {
		t.Fatalf("prepared = %+v", prepared)
	}
	rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, true)
	res, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest)
	if err != nil || res.Replayed || res.Generation != 2 || res.RelationshipID != ridC || res.ManifestDigest != prepared.ManifestDigest || res.OpenedBy != OpenedByGenerationOpen ||
		res.DispatchRequestID != prepared.DispatchRequestID || res.DispatchTurnID != "turn-dispatch-"+ridC {
		t.Fatalf("dag-correct for the hand-opened generation = %v %+v", err, res)
	}
	var kind, bound string
	var request sql.NullString
	if err := k.s.DB.QueryRow("SELECT kind, manifest_digest, managed_request_id FROM dag_node_executions WHERE relationship_id = ? AND execution_generation = 2", ridC).Scan(&kind, &bound, &request); err != nil ||
		kind != "correction" || bound != prepared.ManifestDigest || request.String != prepared.DispatchRequestID {
		t.Fatalf("the execution row = %s %s %v %v", kind, bound, request, err)
	}
	var childAfter string
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", ridC).Scan(&childAfter); err != nil || childAfter != childBefore {
		t.Fatalf("the correction went to %q, the child of C is %q (%v)", childAfter, childBefore, err)
	}
	if got := k.rvFleet(); !strings.HasPrefix(got, prefix) {
		t.Fatalf("the hand-opened correction made a relationship or a child:\nbefore %s\nafter  %s", fleet, got)
	}
	if got := rvAction(t, k.read("sr"), "C"); got != rvHold {
		t.Fatalf("the route of C while generation 2 is open = %q, want %q", got, rvHold)
	}
	// recording again is a replay that says how the generation was opened; another digest is refused
	if again, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); err != nil || !again.Replayed || again.OpenedBy != OpenedByGenerationOpen || again.DispatchTurnID != res.DispatchTurnID {
		t.Fatalf("replay = %v %+v", err, again)
	}
	if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", dig("another manifest")); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("replay with another digest = %v", err)
	}

	// the child reports the reworked output in the bound generation and the parent accepts it in place of the first one
	k.rvReportGeneration(ridC, "C", 2, releaseCriteriaDigest())
	second, err := k.accept("sr", "C", AcceptInput{Supersedes: accepted["C"].AcceptanceID})
	if err != nil || second.SupersededID != accepted["C"].AcceptanceID || second.Generation != 2 || second.AcceptanceID == accepted["C"].AcceptanceID {
		t.Fatalf("accept of the reworked C = %v %+v", err, second)
	}
	after := k.read("sr")
	if got := invStaleIDs(after); len(got) != 0 {
		t.Fatalf("after C was reworked %v are still stale: %s", got, after.brief())
	}
	if got := k.rvFleet(); !strings.HasPrefix(got, prefix) {
		t.Fatalf("the rework made a relationship or a child:\nbefore %s\nafter  %s", fleet, got)
	}
	for node, before := range siblings {
		if now := rvRecords(k, "sr", node); now != before {
			t.Fatalf("the records of the unrelated node %s changed:\nbefore %s\nafter  %s", node, before, now)
		}
	}
}

// What dag-correct will not bind for a generation opened by hand: one opened for another manifest, one nobody bound to a turn, one without a named manifest, a manifest that is not the node's, and a
// node whose result is not stale (those are corrected by a ruling). Each leaves no execution row.
func TestAGenerationOpenedByHandIsBoundOnlyToTheManifestItWasOpenedFor(t *testing.T) {
	bound := func(k *releaseKit) int {
		return k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'C' AND execution_generation = 2")
	}
	t.Run("opened under another request id", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, "some-other-request", true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "not opened for this manifest") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
	})
	t.Run("opened for a manifest prepared before the plan moved", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, true)
		k.invRevise("sr", "C", "sr-r3", invTitle("C revised after the prepare"))
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "open no further generation") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
		// the procedure is to report the refusal and open no further generation. A coordinator that disregards it finds generation 2 still open and unrecorded: preparing again names generation 3, and
		// recording that one is refused at the gap with the relay's own words, so nothing is bound either way
		again := k.rvPrepare("sr", "C")
		if again.ManifestDigest == prepared.ManifestDigest || !strings.Contains(again.Instruction, "generation 3") {
			t.Fatalf("the second prepare = %+v", again)
		}
		reg := &registry.Registry{Store: k.s}
		if _, err := reg.OpenGeneration(context.Background(), ridC, again.DispatchRequestID, "needs_changes_revision", sql.NullString{String: "turn-dispatch-3", Valid: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", again.ManifestDigest); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "follows generation 2") ||
			k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'C' AND execution_generation > 1") != 0 {
			t.Fatalf("the second correction = %v", err)
		}
	})
	t.Run("no turn bound to the generation yet", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, false)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "generation-bind") {
			t.Fatalf("the refusal does not name the way on: %v", err)
		}
		if _, err := (&registry.Registry{Store: k.s}).BindAnchor(context.Background(), ridC, 2, "turn-dispatch-late", "dispatch_receipt"); err != nil {
			t.Fatal(err)
		}
		if res, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); err != nil || res.DispatchTurnID != "turn-dispatch-late" {
			t.Fatalf("after the bind = %v %+v", err, res)
		}
	})
	t.Run("no manifest named", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", ""); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "--manifest-digest") {
			t.Fatalf("the refusal does not ask for the manifest: %v", err)
		}
	})
	t.Run("a manifest that is not stored for the node", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", dig("invented")); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "not stored for node") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
	})
	t.Run("opened for one manifest, bound to another valid one", func(t *testing.T) {
		k, _, ridC, first := rvHandKit(t)
		second := k.rvPrepareNotes("sr", "C", "a second set of notes")
		if second.ManifestDigest == first.ManifestDigest || second.DispatchRequestID == first.DispatchRequestID {
			t.Fatalf("two manifests of one node and slice must differ: %+v %+v", first, second)
		}
		rvOpenByHand(t, k, ridC, first.DispatchRequestID, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", second.ManifestDigest); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction with the other manifest = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "not opened for this manifest") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
		if res, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", first.ManifestDigest); err != nil || res.ManifestDigest != first.ManifestDigest {
			t.Fatalf("correction with the manifest it was opened for = %v %+v", err, res)
		}
	})
	t.Run("a correction already recorded is not skipped by opening another beside it", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); err != nil {
			t.Fatal(err)
		}
		next := k.rvPrepare("sr", "C")
		if !strings.Contains(next.Instruction, "generation 3") {
			t.Fatalf("the second prepare = %+v", next)
		}
		reg := &registry.Registry{Store: k.s}
		if _, err := reg.OpenGeneration(context.Background(), ridC, next.DispatchRequestID, "needs_changes_revision", sql.NullString{String: "turn-dispatch-3", Valid: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", next.ManifestDigest); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "already open") ||
			k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'C' AND execution_generation = 3") != 0 {
			t.Fatalf("a second correction beside the open one = %v", err)
		} else if !strings.Contains(err.Error(), "open no further generation") || strings.Contains(err.Error(), "wait for its report") {
			// opening the second generation moved the relationship past the one the first correction is accepted on, so waiting for that report is not a way on: the refusal must say to report and stop
			t.Fatalf("the refusal offers a way on that is not there: %v", err)
		}
	})
	t.Run("an input accepted again between the prepare and the recording", func(t *testing.T) {
		k, _, ridC, prepared := rvHandKit(t)
		rvOpenByHand(t, k, ridC, prepared.DispatchRequestID, true)
		// A is revised and accepted a third time after C's manifest was prepared and its generation opened: the slice and the criteria of C are as they were, and the manifest names an acceptance of A that
		// is superseded now. Binding it would let C report and be accepted on an input that was replaced before it began
		var active string
		if err := k.s.DB.QueryRow("SELECT acceptance_id FROM dag_acceptances WHERE plan_id = 'sr' AND node_id = 'A' AND state = 'active'").Scan(&active); err != nil {
			t.Fatal(err)
		}
		k.invRevise("sr", "A", "sr-r3", invTitle("A revised again"))
		inputs := invRealInputs(k, "sr", "A")
		k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE acceptance_id = ?", active)
		k.acceptNode("sr", "A", acceptOpts{Suffix: "-3", Inputs: inputs})
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "C", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" || bound(k) != 0 {
			t.Fatalf("correction = %v (%d bound)", err, bound(k))
		} else if !strings.Contains(err.Error(), "does not rest on the inputs") || !strings.Contains(err.Error(), "open no further generation") {
			t.Fatalf("the refusal does not say why or what to do: %v", err)
		}
	})
	t.Run("a change of the criteria alone is revalidated, not corrected by hand", func(t *testing.T) {
		k, accepted := rvSettledSharedRoot(t)
		k.rvReregister("sr", "A", "sr-r2", accepted["A"].RelationshipID, dig("A's new criteria"), nil)
		if got := rvAction(t, k.read("sr"), "A"); got != rvRevalidate {
			t.Fatalf("the route of A = %q, want %q", got, rvRevalidate)
		}
		ridA := accepted["A"].RelationshipID
		prepared := k.rvPrepare("sr", "A")
		rvOpenByHand(t, k, ridA, prepared.DispatchRequestID, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "A", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" || k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'A' AND execution_generation = 2") != 0 {
			t.Fatalf("correction = %v", err)
		} else if !strings.Contains(err.Error(), "revalidate") {
			t.Fatalf("the refusal does not name the route: %v", err)
		}
	})
	t.Run("a node whose accepted result is current has no hand-opened route", func(t *testing.T) {
		k, accepted, _, _ := rvHandKit(t)
		ridB := accepted["B"].RelationshipID
		prepared := k.rvPrepare("sr", "B")
		rvOpenByHand(t, k, ridB, prepared.DispatchRequestID, true)
		if _, err := k.sched.RecordCorrection(context.Background(), "sr", "B", "parent", prepared.ManifestDigest); refusalReason(err) != "disposition_conflict" || k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'B' AND execution_generation = 2") != 0 {
			t.Fatalf("correction = %v", err)
		} else if !strings.Contains(err.Error(), "not stale") {
			t.Fatalf("the refusal does not say why: %v", err)
		}
	})
}
