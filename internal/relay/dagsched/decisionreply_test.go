package dagsched

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// CRW-433: a split approval or scope change returned with decision-reply opens the next generation of the SAME relationship, and dag-correct binds that generation to the node so that its result is
// accepted. These tests drive the relay's own decision writer (delivery.Ack.RecordDecision) over the real release path and the scheduler's own correction and accept functions; the words dag-correct
// prints are literals so that a renamed constant cannot silently rename them. Synthetic data only.

const drReduced = "preserve the contract, first part only"

// drWorld is a node released to a child that then stopped with a blocked_needs_input receipt of generation 1.
type drWorld struct {
	k       *releaseKit
	plan    string
	node    string
	rid     string
	child   string
	blocked string
	ack     *delivery.Ack
}

// newDRWorld releases node A of the release plan; newDRWorldOn releases another node (B first settles A, so it rests on an accepted input).
func newDRWorld(t *testing.T) *drWorld { return newDRWorldOn(t, "A") }

func newDRWorldOn(t *testing.T, node string) *drWorld {
	t.Helper()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "dp")
	if node == "B" {
		k.invSettle("dp", "A")
	}
	res := k.mustRelease("dp", node)
	w := &drWorld{k: k, plan: "dp", node: node, rid: res.RelationshipID, ack: delivery.NewAck(delivery.NewService(k.s, delivery.SystemClock{}))}
	if err := k.s.DB.QueryRow("SELECT child_task_id FROM relationships WHERE relationship_id = ?", w.rid).Scan(&w.child); err != nil {
		t.Fatal(err)
	}
	// the decision is routed to the child, so the child is an allowed recipient
	k.exec("UPDATE relationships SET allowed_recipients = ? WHERE relationship_id = ?", `["parent","`+w.child+`"]`, w.rid)
	now := k.clock()
	w.blocked = "evt-blocked-" + w.rid
	k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
		" VALUES (?, ?, 1, ?, 'blocked_needs_input', 'child', ?, 'turn-blocked', 'completed', '{}', 'final', ?, ?)", w.blocked, w.rid, dig("blocked "+w.rid), w.child, now, now)
	return w
}

func drDigest() string {
	return delivery.SetDigest([]delivery.Criterion{{ID: "c1", Title: drReduced, Required: true}})
}

// register is criteria-register of the reduced set: the set the child is to continue under.
func (w *drWorld) register() {
	w.k.t.Helper()
	if _, err := w.ack.Criteria.Register(context.Background(), w.rid, []any{delivery.Obj{{Key: "id", Value: "c1"}, {Key: "title", Value: drReduced}}}, nil); err != nil {
		w.k.t.Fatal(err)
	}
}

// revise is the plan revision that narrows the node's criteria to the reduced set.
func (w *drWorld) revise(request string) {
	w.k.t.Helper()
	w.k.invRevise(w.plan, w.node, request, func(n doc) { n["criteria_set_digest"] = drDigest() })
}

// decide is decision-reply with the given kind, and the generation it opened.
func (w *drWorld) decide(kind, digest string) (string, error) {
	w.k.t.Helper()
	record, err := w.ack.RecordDecision(context.Background(), delivery.DecisionRequest{EventID: w.blocked, Decision: kind, Turn: "decision-turn", Note: "split the work: this node keeps the first part", CriteriaDigest: digest})
	if err != nil {
		return "", err
	}
	for _, f := range record {
		if f.Key == "eventId" {
			return fmt.Sprint(f.Value), nil
		}
	}
	return "", fmt.Errorf("the decision record names no event: %v", record)
}

func (w *drWorld) mustDecide(kind, digest string) string {
	w.k.t.Helper()
	id, err := w.decide(kind, digest)
	if err != nil {
		w.k.t.Fatalf("decision-reply %s: %v", kind, err)
	}
	return id
}

// deliver is the delivery engine having dispatched the decision message into a turn of the child: the generation the reply opened is bound to it.
func (w *drWorld) deliver(generation int64) {
	w.k.t.Helper()
	w.k.exec("UPDATE generations SET anchor_state = 'bound', dispatch_turn_id = 'turn-decision', bound_at = ? WHERE relationship_id = ? AND execution_generation = ?", w.k.clock(), w.rid, generation)
}

func (w *drWorld) correct(actor, digest string) (CorrectionResult, error) {
	w.k.t.Helper()
	return w.k.sched.RecordCorrection(context.Background(), w.plan, w.node, actor, digest)
}

// executions is the rows dag-correct and release leave for the node, as text.
func (w *drWorld) executions() string {
	return rvRows(w.k, "SELECT relationship_id, execution_generation, manifest_digest, kind, COALESCE(managed_request_id, '') FROM dag_node_executions WHERE plan_id = ? AND node_id = ? ORDER BY execution_generation", w.plan, w.node)
}

func (w *drWorld) manifest(digest string) map[string]any {
	w.k.t.Helper()
	body, found, err := w.k.repo.ReadManifest(context.Background(), digest)
	if err != nil || !found {
		w.k.t.Fatalf("manifest %s: %v %v", digest, found, err)
	}
	return body
}

// Criterion c1: after a blocked_needs_input receipt the parent's split approval is delivered and recorded through the relay, the child continues on the same node under the reduced criteria, and the DAG
// accepts the node's result in the generation the decision opened: it is recorded as an execution of the node, never refused as stale, and no new child is made.
func TestSplitApprovalGenerationIsRecordedAndItsResultAccepted(t *testing.T) {
	w := newDRWorld(t)
	k := w.k
	var previous string
	if err := k.s.DB.QueryRow("SELECT manifest_digest FROM dag_node_executions WHERE node_id = 'A' AND execution_generation = 1").Scan(&previous); err != nil {
		t.Fatal(err)
	}
	w.revise("dp-r2")
	w.register()
	decision := w.mustDecide("split_approval", drDigest())
	w.deliver(2)
	if n := k.count("SELECT COUNT(*) FROM generations WHERE relationship_id = ? AND reason = 'decision_reply' AND dispatch_request_id = ?", w.rid, "decision-"+decision); n != 1 {
		t.Fatalf("the split approval did not open generation 2 under its own request id (%d)", n)
	}
	// the child continues in generation 2 and reports; the relay ruled it verified under the reduced set
	k.rvReportGeneration(w.rid, "A", 2, drDigest())

	t.Run("the generation is not an execution of the node until dag-correct records it", func(t *testing.T) {
		if _, err := k.accept("dp", "A", AcceptInput{}); refusalReason(err) != "stale_generation" {
			t.Fatalf("accept before dag-correct = %v, want stale_generation", err)
		}
	})

	res, err := w.correct("parent", "")
	if err != nil {
		t.Fatalf("dag-correct of the generation the decision opened = %v", err)
	}
	if res.Generation != 2 || res.RelationshipID != w.rid || res.Replayed || res.CarriedOver || res.OpenedBy != "decision_reply" || res.DispatchRequestID != "decision-"+decision || res.DispatchTurnID != "turn-decision" {
		t.Fatalf("dag-correct = %+v", res)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = 'A' AND execution_generation = 2 AND kind = 'correction' AND manifest_digest = ? AND managed_request_id = ?", res.ManifestDigest, "decision-"+decision); got != 1 {
		t.Fatalf("generation 2 is not recorded as the correction execution of A:\n%s", w.executions())
	}

	// the manifest is the node as the plan holds it now, with what the child was dispatched with
	before, after := w.manifest(previous), w.manifest(res.ManifestDigest)
	if after["criteria_set_digest"] != drDigest() || before["criteria_set_digest"] == drDigest() || after["manifest_digest"] == before["manifest_digest"] {
		t.Fatalf("criteria of the manifests: before %v, after %v", before["criteria_set_digest"], after["criteria_set_digest"])
	}
	snap := k.snapshot("dp")
	if n, _ := nodeOf(snap, "A"); after["node_slice_digest"] != n.SliceDigest || fmt.Sprint(after["plan_revision_no"]) != fmt.Sprint(snap.Revision) {
		t.Fatalf("the manifest is not the node at the plan revision it holds now: %v", after)
	}
	for _, key := range []string{"inputs", "base", "volatile", "rule_version"} {
		if dag.Canonical(before[key]) != dag.Canonical(after[key]) {
			t.Fatalf("%s changed: %s -> %s", key, dag.Canonical(before[key]), dag.Canonical(after[key]))
		}
	}
	if n := k.read("dp").node("A"); n.State != StateRunning && n.State != StateReported {
		t.Fatalf("A while its child continues = %+v", n)
	}

	// the same call again is a replay, and a supplied digest only cross-checks
	if again, err := w.correct("parent", res.ManifestDigest); err != nil || !again.Replayed || again.OpenedBy != "decision_reply" || again.ManifestDigest != res.ManifestDigest || again.DispatchTurnID != "turn-decision" {
		t.Fatalf("replay = %v %+v", err, again)
	}
	if _, err := w.correct("parent", dig("another manifest")); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a digest other than the bound one = %v", err)
	}

	accepted, err := k.accept("dp", "A", AcceptInput{})
	if err != nil || accepted.Generation != 2 || accepted.Revalidated || accepted.Replayed || accepted.SupersededID != "" {
		t.Fatalf("accept of the result of generation 2 = %v %+v", err, accepted)
	}
	reading := k.read("dp")
	if n := reading.node("A"); n.Reason != DoneAccepted {
		t.Fatalf("A after its accepted result = %+v", n)
	}
	if got := invStaleIDs(reading); len(got) != 0 {
		t.Fatalf("the criteria change the decision carried reads as stale: %v: %s", got, reading.brief())
	}
	if k.count("SELECT COUNT(*) FROM relationships WHERE issue_key = 'CRW-A'") != 1 {
		t.Fatal("the decision made another child")
	}
}

// A scope change opens a generation the same way, and a decision that keeps its generation (answer, stop) opens none: there is nothing for dag-correct to bind.
func TestDecisionKindsFixWhatTheDAGRecords(t *testing.T) {
	t.Run("scope_change", func(t *testing.T) {
		w := newDRWorld(t)
		w.revise("dp-r2")
		w.register()
		decision := w.mustDecide("scope_change", drDigest())
		w.deliver(2)
		res, err := w.correct("parent", "")
		if err != nil || res.OpenedBy != "decision_reply" || res.DispatchRequestID != "decision-"+decision {
			t.Fatalf("dag-correct of a scope change = %v %+v", err, res)
		}
	})
	for _, kind := range []string{"answer", "stop"} {
		t.Run(kind, func(t *testing.T) {
			w := newDRWorld(t)
			w.mustDecide(kind, "")
			if g := w.k.count("SELECT execution_generation FROM relationships WHERE relationship_id = ?", w.rid); g != 1 {
				t.Fatalf("a %s moved the generation to %d", kind, g)
			}
			before := w.executions()
			res, err := w.correct("parent", "")
			if err != nil || !res.Replayed || res.Generation != 1 || w.executions() != before {
				t.Fatalf("dag-correct after a %s = %v %+v, want the recorded first generation replayed and nothing written", kind, err, res)
			}
		})
	}
}

// What dag-correct refuses for a generation a decision opened. Each refusal writes nothing; the existing reasons are reused (no reason is added).
func TestDecisionGenerationIsNotRecordedUnlessTheChildWasToldAndThePlanHoldsWhatItWasToldOn(t *testing.T) {
	cases := []struct {
		name   string
		setup  func(w *drWorld)
		actor  string
		reason string
		said   string
	}{
		{name: "the decision has not reached the child", reason: "disposition_conflict", said: "no dispatch turn", actor: "parent",
			setup: func(w *drWorld) { w.revise("dp-r2"); w.register(); w.mustDecide("split_approval", drDigest()) }},
		{name: "the plan still holds the criteria before the split", reason: "criteria_set_changed", said: drDigest(), actor: "parent",
			setup: func(w *drWorld) { w.register(); w.mustDecide("split_approval", drDigest()); w.deliver(2) }},
		{name: "only the parent of the relationship records it", reason: "scope_role_mismatch", actor: "someone-else",
			setup: func(w *drWorld) {
				w.revise("dp-r2")
				w.register()
				w.mustDecide("split_approval", drDigest())
				w.deliver(2)
			}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := newDRWorld(t)
			c.setup(w)
			before := w.executions()
			_, err := w.correct(c.actor, "")
			if refusalReason(err) != c.reason || (c.said != "" && !strings.Contains(err.Error(), c.said)) {
				t.Fatalf("dag-correct = %v, want %s naming %q", err, c.reason, c.said)
			}
			if after := w.executions(); after != before {
				t.Fatalf("a refusal wrote:\nbefore %s\nafter  %s", before, after)
			}
		})
	}

	t.Run("the plan is revised, then the same call goes through", func(t *testing.T) {
		w := newDRWorld(t)
		w.register()
		w.mustDecide("split_approval", drDigest())
		w.deliver(2)
		if _, err := w.correct("parent", ""); refusalReason(err) != "criteria_set_changed" {
			t.Fatalf("before the plan names the reduced criteria = %v", err)
		}
		w.revise("dp-r2")
		if res, err := w.correct("parent", ""); err != nil || res.OpenedBy != "decision_reply" {
			t.Fatalf("after the plan names them = %v %+v", err, res)
		}
	})

	t.Run("what the child consumed moved since it was dispatched", func(t *testing.T) {
		w := newDRWorldOn(t, "B")
		w.revise("dp-r2")
		w.register()
		w.mustDecide("split_approval", drDigest())
		w.deliver(2)
		// the accepted result B consumed is superseded: the manifest the child was dispatched with rests on an input that is not there any more
		w.k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE node_id = 'A'")
		before := w.executions()
		if _, err := w.correct("parent", ""); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "child was not told") {
			t.Fatalf("dag-correct = %v, want disposition_conflict saying the child was not told what moved", err)
		}
		if after := w.executions(); after != before {
			t.Fatalf("a refusal wrote:\nbefore %s\nafter  %s", before, after)
		}
	})
}
