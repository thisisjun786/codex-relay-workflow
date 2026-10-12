package dagsched

import (
	"context"
	"testing"
)

// CRW-1205 d7: a flow run that is cancelled with no process gives back what it owned. CancelRunNodes closes the current agent execution
// of every node the run released, returns each slot, skips a node that has none, and is a replay when it is called again.
func TestCancelRunNodesReturnsTheSlotsOfTheRun(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	a := k.mustReleaseAgent("rp", "A")
	i := k.mustReleaseAgent("rp", "I")
	held := func() int {
		return k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND state = 'held'")
	}
	if held() != 2 {
		t.Fatalf("%d slots held after two releases", held())
	}
	nodes := []RunNode{{"rp", "A"}, {"rp", "I"}, {"rp", "J"}}
	res, err := k.sched.CancelRunNodes(context.Background(), nodes)
	if err != nil || len(res) != 2 {
		t.Fatalf("CancelRunNodes: %+v %v", res, err)
	}
	for _, r := range res {
		if !r.Closed || !r.SlotReleased || r.State != AgentClosureCancelled || r.Reason != AgentReasonFlowCancel {
			t.Errorf("a close: %+v", r)
		}
	}
	if held() != 0 {
		t.Errorf("%d slots still held after the cancel", held())
	}
	for _, id := range []string{a.ExecutionID, i.ExecutionID} {
		if n := k.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_kind = 'execution' AND subject_id = ? AND state = 'cancelled' AND reason = 'flow_cancel' AND slot_released = 1", id); n != 1 {
			t.Errorf("execution %s has %d cancelled closures", id, n)
		}
	}
	// again: nothing is open, nothing is written
	before := k.count("SELECT COUNT(*) FROM dag_agent_closures")
	if res, err := k.sched.CancelRunNodes(context.Background(), nodes); err != nil || len(res) != 0 || k.count("SELECT COUNT(*) FROM dag_agent_closures") != before {
		t.Errorf("a second cancel: %+v %v", res, err)
	}
	// the nodes are free for the next admission
	if _, err := k.releaseAgent("rp", "A"); err != nil {
		t.Errorf("A cannot be released again after the cancel: %v", err)
	}
}
