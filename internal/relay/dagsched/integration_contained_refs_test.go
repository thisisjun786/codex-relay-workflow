package dagsched

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-965 (revision of parent decision D4): a commit-accepted node whose head a later batch found already contained moves
// no ref in that batch, but it still completes on the integration branch that contains it. Y is contained in X's history;
// the named batch merges X, the next run marks Y with no merge and no verification, and observing Y on the branch then
// satisfies Y's integrated edge to Z.
func TestContainedNodeCompletesOnTheBranchThatContainsIt(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "y", files: map[string]string{"y.txt": "y\n"}})
	k.addChildNode("x", k.heads["y"], "x.txt")
	k.putPlan("g", int(k.snapshot("g").Revision), "g-z", addRelNode("z", dag.NodeImplementation), addEdge("yz", "y", "z", dag.EdgeIntegrated, doc{"target_repository": k.repo.path}))
	k.acceptByCommit("y")
	k.acceptByCommit("x")
	ctx := context.Background()
	k.sched.Tips = newLegacyLocalTipReader(t)
	k.sched.Ancestry = GitAncestry{}.Ancestry
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	first := k.batchIn()
	first.Nodes = []string{"x"}
	if _, err := k.sched.IntegrateBatch(ctx, first, counting); err != nil {
		t.Fatalf("named batch: %v", err)
	}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
	if err != nil {
		t.Fatalf("next batch: %v", err)
	}
	if runs != 1 || len(res.Merged) != 0 {
		t.Fatalf("verifier ran %d times and merged %d; the contained node needs neither", runs, len(res.Merged))
	}
	acc, found, err := loadActiveAcceptance(ctx, k.sched.Store.Q(ctx), "g", "y")
	if err != nil || !found {
		t.Fatalf("the active acceptance of y: %v, %v", found, err)
	}
	refs, err := integrationRefsOf(ctx, k.sched.Store.Q(ctx), acc.AcceptanceID)
	if err != nil || len(refs) != 1 || refs[0] != "dev-int" {
		t.Fatalf("the completion refs of a contained node: %v (err %v); want dev-int", refs, err)
	}
	obs, err := k.sched.ObserveIntegration(ctx, "g", "y", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
	if err != nil || !obs.Integrated {
		t.Fatalf("observe y on the integration branch: %+v, %v; want integrated", obs, err)
	}
	snap := k.snapshot("g")
	for _, e := range snap.Edges {
		if e.EdgeID == "yz" {
			st, err := k.sched.edgeStatus(ctx, k.sched.Store.Q(ctx), "g", snap, e)
			if err != nil || !st.Satisfied {
				t.Fatalf("the integrated edge y to z: %+v (err %v); want satisfied once y is observed", st, err)
			}
			return
		}
	}
	t.Fatal("the plan has no edge yz")
}

// CRW-965 (revision of parent decision D4): a node whose only batch failed before its move names no ref, so its edge
// waits; the failed attempt leaves nothing that a completion could read.
func TestNodeWhoseOnlyBatchFailedBeforeItsMoveNamesNoRef(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "p", files: map[string]string{"p.txt": "p\n"}})
	k.acceptByCommit("p")
	ctx := context.Background()
	crash := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(context.Context, string, string, string, string) error {
		return errors.New("simulated refusal before the branch moved")
	}}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), crash); err == nil {
		t.Fatal("the failing batch should not complete")
	}
	acc, found, err := loadActiveAcceptance(ctx, k.sched.Store.Q(ctx), "g", "p")
	if err != nil || !found {
		t.Fatalf("the active acceptance of p: %v, %v", found, err)
	}
	refs, err := integrationRefsOf(ctx, k.sched.Store.Q(ctx), acc.AcceptanceID)
	if err != nil || len(refs) != 0 {
		t.Fatalf("the completion refs of a node whose batch never moved: %v (err %v); want none", refs, err)
	}
}
