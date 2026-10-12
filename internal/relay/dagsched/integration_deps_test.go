package dagsched

import (
	"context"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// TestSchedRelayMilestoneOrderMatchesReady (CRW-1277 c, the scheduler's half): OfferingOrder, the order the run's milestone lists its
// nodes in, is the order Ready offers the nodes that are ready together: the longest chain below a node first, then the most nodes
// below it, then the node id; the run's own test (internal/flow/cli) reads a milestone through it.
func TestSchedRelayMilestoneOrderMatchesReady(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	// R1, R2 and R3 are ready together; two nodes hang below R2 in a chain, one below R3, none below R1
	f.putPlan("order", 0, "order-r1", addRelNode("R1", dag.NodeNonPR), addRelNode("R2", dag.NodeNonPR), addRelNode("R3", dag.NodeNonPR),
		addRelNode("S1", dag.NodeNonPR), addRelNode("S2", dag.NodeNonPR), addRelNode("T1", dag.NodeNonPR),
		addEdge("r2s1", "R2", "S1", dag.EdgeArtifactVerified, nil), addEdge("s1s2", "S1", "S2", dag.EdgeArtifactVerified, nil), addEdge("r3t1", "R3", "T1", dag.EdgeArtifactVerified, nil))
	order := OfferingOrder(f.snapshot("order"))
	if want := []string{"R2", "R3", "S1", "R1", "S2", "T1"}; !slices.Equal(order, want) {
		t.Fatalf("the offering order is %v, want %v", order, want)
	}
	reading, err := f.sched.Read(context.Background(), "order", ReadyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ready []string
	for _, n := range reading.Ready {
		ready = append(ready, n.NodeID)
	}
	var offered []string
	for _, id := range order {
		if slices.Contains(ready, id) {
			offered = append(offered, id)
		}
	}
	if len(ready) != 3 || !slices.Equal(ready, offered) {
		t.Fatalf("Ready offers %v, the offering order lists them as %v (%s)", ready, offered, reading.brief())
	}
}
