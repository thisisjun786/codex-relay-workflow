package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-965 (parent decision, D4): a commit-accepted node completes on the local integration branch its batch moved. Its
// outgoing integrated edge names the checkout; the edge's base ref is not part of what integrated means for such a node,
// so the successor becomes satisfied once the node is observed on the integration branch.
func TestCommitAcceptedNodeCompletesOnTheIntegrationBranchItMoved(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.putPlan("g", 1, "g-r2", addRelNode("b", dag.NodeNonPR),
		addEdge("ab", "a", "b", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "dev"}))
	ctx := context.Background()
	k.acceptByCommit("a")
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	k.sched.Tips = newLegacyLocalTipReader(t)
	k.sched.Ancestry = GitAncestry{}.Ancestry
	obs, err := k.sched.ObserveIntegration(ctx, "g", "a", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
	if err != nil || !obs.Integrated {
		t.Fatalf("observe a on the integration branch: %+v, %v; want integrated", obs, err)
	}
	if st := k.commitEdgeStatusOf(t, "ab"); !st.Satisfied {
		t.Fatalf("the successor should be satisfied once a landed on the integration branch, got %+v", st)
	}
}
