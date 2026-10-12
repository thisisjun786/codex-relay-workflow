package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
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
	nodes := []RunNode{{"rp", "A", a.ExecutionID}, {"rp", "I", i.ExecutionID}, {"rp", "J", ""}}
	res, err := k.sched.CancelRunNodes(context.Background(), "run-1", nodes)
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
	if res, err := k.sched.CancelRunNodes(context.Background(), "run-1", nodes); err != nil || len(res) != 0 || k.count("SELECT COUNT(*) FROM dag_agent_closures") != before {
		t.Errorf("a second cancel: %+v %v", res, err)
	}
	// the nodes are free for the next admission
	if _, err := k.releaseAgent("rp", "A"); err != nil {
		t.Errorf("A cannot be released again after the cancel: %v", err)
	}
}

// CRW-1205 d7 (second round): a cancel closes only what its own run owns. Run 1 releases A and cancels; run 2 releases A again; the
// first cancel again (the documented second `crw flow cancel` of a cancelled run) leaves run 2's execution and slot alone.
func TestCancelRunNodesLeavesTheExecutionOfAnotherRun(t *testing.T) {
	t.Parallel()
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	first := k.mustReleaseAgent("rp", "A")
	nodes := []RunNode{{Plan: "rp", Node: "A", Execution: first.ExecutionID}}
	if res, err := k.sched.CancelRunNodes(context.Background(), "run-1", nodes); err != nil || len(res) != 1 || !res[0].Closed {
		t.Fatalf("the first cancel of run-1: %+v %v", res, err)
	}
	n, _ := nodeOf(k.snapshot("rp"), "A")
	second, err := k.sched.ReleaseAgent(context.Background(), "rp", "A", "parent", k.agentRequest(n.Kind == dag.NodeImplementation), AgentReleaseOptions{RunID: "run-2", Worktree: "/wt/A"})
	if err != nil {
		t.Fatalf("run-2 releases A: %v", err)
	}
	held := func() int {
		return k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND state = 'held'")
	}
	if held() != 1 {
		t.Fatalf("%d slots held after run-2 released A", held())
	}
	closures := k.count("SELECT COUNT(*) FROM dag_agent_closures")
	for _, again := range [][]RunNode{nodes, {{Plan: "rp", Node: "A"}}} {
		res, err := k.sched.CancelRunNodes(context.Background(), "run-1", again)
		if err != nil || len(res) != 0 {
			t.Errorf("a cancel of run-1 after run-2 released the node: %+v %v", res, err)
		}
	}
	if got := k.count("SELECT COUNT(*) FROM dag_agent_closures"); got != closures {
		t.Errorf("closures went from %d to %d: the cancel of run-1 closed run-2's execution %s", closures, got, second.ExecutionID)
	}
	if held() != 1 {
		t.Errorf("%d slots held: run-2's slot was taken", held())
	}
	// run 2's own cancel still gives its execution up
	res, err := k.sched.CancelRunNodes(context.Background(), "run-2", []RunNode{{Plan: "rp", Node: "A", Execution: second.ExecutionID}})
	if err != nil || len(res) != 1 || res[0].ExecutionID != second.ExecutionID || !res[0].Closed || !res[0].SlotReleased {
		t.Fatalf("the cancel of run-2: %+v %v", res, err)
	}
	if held() != 0 {
		t.Errorf("%d slots held after run-2's cancel", held())
	}
}

// A node the run released and a replacement parent took over (a successor of the same run) is still the run's: the cancel names the
// execution of the release and closes the handover row that follows it.
func TestCancelRunNodesClosesTheSameRunSuccessor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	spec := agentSpec{name: "a", files: fileOf("a")}
	k := newAgentKit(t, spec)
	first := k.releases["a"]
	s2 := k.orchestrator("parent-2", "session-2")
	adopted, err := s2.Adopt(ctx, "g", "a", "parent-2")
	if err != nil || adopted.ExecutionID == "" || adopted.ExecutionID == first.ExecutionID {
		t.Fatalf("adopt = %+v, %v", adopted, err)
	}
	if res, err := s2.CancelRunNodes(ctx, "run-2", []RunNode{{Plan: "g", Node: "a", Execution: first.ExecutionID}}); err != nil || len(res) != 0 {
		t.Fatalf("another run's cancel closed the handover row: %+v %v", res, err)
	}
	res, err := s2.CancelRunNodes(ctx, "run-1", []RunNode{{Plan: "g", Node: "a", Execution: first.ExecutionID}})
	if err != nil || len(res) != 1 || res[0].ExecutionID != adopted.ExecutionID || !res[0].Closed || !res[0].SlotReleased {
		t.Fatalf("the cancel of the run: %+v %v", res, err)
	}
	// an execution that is not in the node's chain is not the run's release: nothing is closed
	k2 := newAgentKit(t, spec)
	if res, err := k2.sched.CancelRunNodes(ctx, "run-1", []RunNode{{Plan: "g", Node: "a", Execution: "dag-unrelated"}}); err != nil || len(res) != 0 {
		t.Fatalf("a cancel naming an execution of another chain: %+v %v", res, err)
	}
}
