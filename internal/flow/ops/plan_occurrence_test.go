package ops

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func TestPlanReadOccurrenceCoalescesAndDistinguishesReversions(t *testing.T) {
	world := newWorld(t)
	world.relay.plan = planOfNodes(map[string]string{"A": "a0", "B": "b0"}, "A", "B")
	world.ledgered(true, designOf("A", "B"))
	read := func(node string) (PlanSeen, string) {
		raw := world.must("planRead", "M", node)
		var seen PlanSeen
		var fields map[string]any
		_ = json.Unmarshal(raw, &seen)
		_ = json.Unmarshal(raw, &fields)
		occurrence, _ := fields["occurrence"].(string)
		return seen, occurrence
	}
	world.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	first, occurrence := read("A")
	if !first.Changed || occurrence == "" {
		t.Fatalf("changed read has no occurrence: %+v occurrence=%q", first, occurrence)
	}
	concurrent, same := read("B")
	if !concurrent.Changed || same != occurrence {
		t.Fatalf("concurrent reads of P1 did not coalesce: %q and %q", occurrence, same)
	}
	world.must("signalBind", "M", 1, "", []Trigger{{Kind: "revision", Call: "plan:" + occurrence + ":" + first.Digest, Node: "A", Digest: first.Digest}})
	if settled, same := read("B"); settled.Changed || same != occurrence {
		t.Fatalf("same occurrence after binding: %+v occurrence=%q, want %q", settled, same, occurrence)
	}
	world.relay.plan = planOfNodes(map[string]string{"A": "a2", "B": "b2"}, "A", "B")
	world.must("signalBind", "M", 2, "", []Trigger{{Kind: "receipt", Call: "receipt", Node: "A", Digest: "receipt"}})
	world.relay.plan = planOfNodes(map[string]string{"A": "a1", "B": "b1"}, "A", "B")
	returned, later := read("B")
	if !returned.Changed || returned.Digest != first.Digest || later == "" || later == occurrence {
		t.Fatalf("return to P1 reused its earlier occurrence: %+v occurrence=%q, earlier=%q", returned, later, occurrence)
	}
	events := world.j.Events()
	readRow := events[len(events)-1]
	operation := &planReadOp{world.lane}
	if proven, _, _ := operation.Proven(context.Background(), events, readRow); proven {
		t.Fatal("an earlier signal for P1 proves a later occurrence of P1")
	}
	if _, same := read("A"); same != later {
		t.Fatalf("concurrent reads of returning P1 did not coalesce: %q and %q", later, same)
	}
	world.must("signalBind", "M", 3, "", []Trigger{{Kind: "revision", Call: "plan:" + later + ":" + returned.Digest, Node: "B", Digest: returned.Digest}})
	if proven, reason, err := operation.Proven(context.Background(), world.j.Events(), readRow); err != nil || !proven {
		t.Fatalf("own signal did not prove returning P1: %v %s %v", proven, reason, err)
	}
}

func TestPlanReadProofMatchesOwnOccurrence(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		readSeq    int
		bindSeq    int
		occurrence string
		want       bool
	}{
		{"own before bind", 1, 2, "own", true},
		{"own after bind", 2, 1, "own", true},
		{"earlier same digest before bind", 1, 2, "earlier", false},
		{"earlier same digest after bind", 2, 1, "earlier", false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			read := engine.Event{Seq: testCase.readSeq, Kind: engine.KindCall, CallID: "read", Op: OpPlanRead, Event: engine.EvCompleted,
				Data: map[string]any{"milestone": "M", "node": "B", "plan_digest": "P1", "changed": true, "occurrence": "own"}}
			bind := engine.Event{Seq: testCase.bindSeq, Kind: engine.KindCall, CallID: "bind", Op: OpSignalBind, Event: engine.EvCompleted,
				Data: map[string]any{"milestone": "M", "members": []Trigger{{Kind: "revision", Call: "plan:" + testCase.occurrence + ":P1", Node: "C", Digest: "P1"}}}}
			events := []engine.Event{read, bind}
			if bind.Seq < read.Seq {
				events = []engine.Event{bind, read}
			}
			operation := &planReadOp{}
			if proven, reason, err := operation.Proven(context.Background(), events, read); err != nil || proven != testCase.want {
				t.Fatalf("Proven=%v %s %v, want %v", proven, reason, err, testCase.want)
			}
		})
	}
}
