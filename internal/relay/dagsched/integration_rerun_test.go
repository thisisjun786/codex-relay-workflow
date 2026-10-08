package dagsched

import (
	"context"
	"reflect"
	"testing"
)

// CRW-965 (review finding): a batch that runs again after a candidate already reached the integration branch (before the
// containment observation) must not merge that candidate again or report a merge commit for it. Only the new candidate
// is merged, verified and reported.
func TestIntegrationBatchDoesNotMergeACandidateTheBranchAlreadyHolds(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	deps := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	ctx := context.Background()
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	first := k.batchIn()
	first.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, first, deps); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), deps)
	if err != nil {
		t.Fatalf("second batch: %v", err)
	}
	var merged []string
	for _, m := range res.Merged {
		merged = append(merged, m.NodeID)
	}
	if !reflect.DeepEqual(merged, []string{"b"}) {
		t.Fatalf("the second batch merged %v; want only [b]: %+v", merged, res)
	}
	if runs != 2 {
		t.Fatalf("verifier ran %d times; want 2 (one per batch)", runs)
	}
}
