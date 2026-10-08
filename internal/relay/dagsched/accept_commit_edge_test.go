package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// commitEdgeStatusOf reads one edge of plan "g" the way the scheduler's readers do.
func (k *batchKit) commitEdgeStatusOf(t *testing.T, edgeID string) EdgeStatus {
	t.Helper()
	ctx := context.Background()
	snap, _, err := dag.SnapshotAt(ctx, k.sched.Store.Q(ctx), "g", 0)
	if err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	for _, e := range snap.Edges {
		if e.EdgeID == edgeID {
			st, err := k.sched.edgeStatus(ctx, k.sched.Store.Q(ctx), "g", snap, e)
			if err != nil {
				t.Fatalf("edge %s: %v", edgeID, err)
			}
			return st
		}
	}
	t.Fatalf("no edge %s", edgeID)
	return EdgeStatus{}
}

// CRW-965 (parent decision d5): a commit-accepted node satisfies a code-pinned artifact_verified edge without a
// pull request, with and without its integration. The acceptance is made by the production commit path.
func TestCommitAcceptedNodeSatisfiesAPinnedArtifactEdge(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.putPlan("g", 1, "g-r2", addRelNode("b", dag.NodeNonPR),
		addEdge("ab", "a", "b", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": k.repo.path, "target_base_ref": "dev"}))
	k.acceptByCommit("a")
	if st := k.commitEdgeStatusOf(t, "ab"); !st.Satisfied {
		t.Fatalf("a commit-accepted node should satisfy the pinned edge before integration, got %+v", st)
	}
	if _, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	if st := k.commitEdgeStatusOf(t, "ab"); !st.Satisfied {
		t.Fatalf("a commit-accepted and integrated node should satisfy the pinned edge, got %+v", st)
	}
}
