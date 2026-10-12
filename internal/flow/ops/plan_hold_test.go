package ops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// CRW-1291: a release the relay refuses for a plan revision the run does not read raises the run-level hold plan_superseded.

// heldRelay refuses every release plan_changed from a head at revision 2 while the run pinned revision 1, and holds an execution of
// node A that a release the flow never recorded left behind (the replay CAS of CRW-1288 refused it), until a close returns its slot.
type heldRelay struct {
	*stubRelay
	execution, run string
	slotHeld       bool
}

func (r *heldRelay) Release(_ context.Context, req ReleaseRequest) (ReleaseResult, error) {
	r.releases = append(r.releases, req)
	res := ReleaseResult{Reason: ReasonPlanChanged, PinnedRevision: 1, HeadRevision: 2}
	if r.slotHeld && req.Node == "A" {
		res.HeldExecution, res.HeldRun = r.execution, r.run
	}
	return res, nil
}

func (r *heldRelay) Close(_ context.Context, req CloseRequest) (CloseResult, error) {
	r.closes = append(r.closes, req)
	released := r.slotHeld && req.Node == "A"
	r.slotHeld = r.slotHeld && !released
	return CloseResult{Closed: released, SlotReleased: released, ExecutionID: r.execution}, nil
}

// holds are the plan_superseded escalation rows of the journal.
func (w *world) holds() []engine.Event {
	var out []engine.Event
	for _, ev := range rows(w.j.Events()).flow(engine.EvEscalation) {
		if dataStr(ev, "reason") == ReasonPlanSuperseded {
			out = append(out, ev)
		}
	}
	return out
}

// TestPlanSupersededHoldReleasesHeldSlot: the hold is raised with the execution the relay still holds for the node. An execution of
// this run (released before the run recorded it) is closed with the hold, so its slot goes back; an execution of another run is not
// the run's to close, and the hold records whose it is. Either way the hold names the execution and what became of its slot.
func TestPlanSupersededHoldReleasesHeldSlot(t *testing.T) {
	for _, owner := range []string{"run-1", "run-other"} {
		world := newWorld(t)
		world.relay.plan = planOfNodes(map[string]string{"A": "a1"}, "A")
		world.ledgered(true, designOf("A"))
		relay := &heldRelay{stubRelay: world.relay, execution: "agent-exec-A", run: owner, slotHeld: true}
		world.lane.d.Relay = relay
		raw, err := world.op("release", "plan-1", "A", "M", map[string]any{"pinned": true})
		if err != nil {
			t.Fatal(err)
		}
		var out ReleaseOutcome
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		holds := world.holds()
		if out.Released || out.Reason != ReasonRelayPlanChanged || len(holds) != 1 || out.Escalation != dataStr(holds[0], "escalation_id") || out.Hold != out.Escalation {
			t.Fatalf("%s: the release %+v and the holds %+v, want one plan_superseded hold the release names", owner, out, holds)
		}
		evidence, _ := holds[0].Data["evidence"].(map[string]any)
		if evidence["held_execution"] != "agent-exec-A" || evidence["held_run"] != owner {
			t.Errorf("%s: the hold records %v, want the execution and the run that owns it", owner, evidence)
		}
		mine := owner == "run-1"
		if relay.slotHeld == mine || (len(relay.closes) == 1) != mine {
			t.Errorf("%s: slot held %v after the closes %+v", owner, relay.slotHeld, relay.closes)
		}
		if mine && (relay.closes[0].Reason != ReasonPlanSuperseded || relay.closes[0].Node != "A" || relay.closes[0].Plan != "plan-1" || evidence["slot"] != "returned") {
			t.Errorf("the run's execution was closed with %+v and the hold says its slot is %v", relay.closes[0], evidence["slot"])
		}
		if !mine && evidence["slot"] != "owned_by_run" {
			t.Errorf("another run's execution: the hold says its slot is %v", evidence["slot"])
		}
	}
}

// TestPlanSupersededHoldStopsTheRunUntilADecision: while the hold stands no node of the run is released (the relay is not asked), and a
// resume asks nothing again (the rows are proven); the parent's decision on the hold lifts it, and the release that raised it stays
// the node's (its lane closes the node by escalation) while a release the hold answered asks again.
func TestPlanSupersededHoldStopsTheRunUntilADecision(t *testing.T) {
	world := newWorld(t)
	world.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	world.ledgered(true, designOf("A", "B"))
	relay := &heldRelay{stubRelay: world.relay}
	world.lane.d.Relay = relay
	world.must("release", "plan-1", "A", "M", map[string]any{"pinned": true})
	var b ReleaseOutcome
	if err := json.Unmarshal(world.must("release", "plan-1", "B", "M", map[string]any{"pinned": true}), &b); err != nil {
		t.Fatal(err)
	}
	holds := world.holds()
	if len(relay.releases) != 1 || b.Released || b.Reason != ReasonPlanSuperseded || b.Escalation != "" || len(holds) != 1 || b.Hold != dataStr(holds[0], "escalation_id") {
		t.Fatalf("a release under the hold: %+v, %d releases asked of the relay, holds %+v", b, len(relay.releases), holds)
	}
	rel := world.releaseRows()
	proven := func(row engine.Event) bool {
		ok, _, err := world.lane.Ops()["release"].Proven(context.Background(), world.j.Events(), row)
		if err != nil {
			t.Fatal(err)
		}
		return ok
	}
	if !proven(rel[0]) || !proven(rel[1]) {
		t.Fatal("a resume would ask the relay again while the hold stands")
	}
	world.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, Event: EvDecisionApplied, Data: map[string]any{"ask": b.Hold, "node": "A", "decision": map[string]any{"id": "dec-1"}}})
	if !proven(rel[0]) || proven(rel[1]) {
		t.Fatal("after the decision the hold's own release must stand and the release it answered must ask again")
	}
}

// TestPlanSupersededHoldOfTheRaisingCall (d1, d3 of the evaluation of 157e0c3d): two nodes refused at once raise two holds. The call that
// raised the older one and runs again (its process ended before the call completed) answers its own hold and closes its execution again.
func TestPlanSupersededHoldOfTheRaisingCall(t *testing.T) {
	world := newWorld(t)
	world.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	world.ledgered(true, designOf("A", "B"))
	relay := &heldRelay{stubRelay: world.relay, execution: "agent-exec-A", run: "run-1", slotHeld: true}
	world.lane.d.Relay = relay
	run := func() (out ReleaseOutcome) {
		res, err := world.lane.Ops()["release"].Run(context.Background(), engine.OpRequest{RunID: "run-1", CallID: "c_A", RequestID: "req-A", Args: json.RawMessage(`["plan-1","A","M",{"pinned":true}]`), Journal: world.j})
		if err != nil || json.Unmarshal(res.Result, &out) != nil {
			t.Fatal(err)
		}
		return out
	}
	own := run().Escalation
	world.j.Append(engine.Event{RunID: "run-1", Kind: engine.KindFlow, CallID: "c_B", Op: OpRelease, Event: engine.EvEscalation, Data: map[string]any{"reason": ReasonPlanSuperseded,
		"ask": ReasonPlanSuperseded, "node": "B", "escalation_id": "esc_B", "release_call": "c_B", "evidence": map[string]any{"plan": "plan-1", "node": "B", "slot": "none"}}})
	if out := run(); out.Escalation != own || out.Reason != ReasonRelayPlanChanged || len(relay.closes) != 2 || relay.closes[1].Node != "A" || relay.closes[1].Plan != "plan-1" {
		t.Fatalf("the raising call again answered %+v (its hold %s) after the closes %+v", out, own, relay.closes)
	}
}
