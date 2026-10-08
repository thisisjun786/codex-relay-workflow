package dagsched

import (
	"context"
	"testing"
)

// CRW-965 review: a node named twice (--node a --node a) is one candidate. Selecting it twice would merge its head twice
// in one batch and write its merged mark twice.
func TestIntegrationBatchCountsADuplicateNodeOnce(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	in := k.batchIn()
	in.Nodes = []string{"a", "a"}
	res, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 1 {
		t.Fatalf("merged %d candidates for a node named twice; want 1: %+v", len(res.Merged), res)
	}
	if n := countStage(k.stageRows(), "marked"); n != 1 {
		t.Fatalf("%d merged marks written; want 1", n)
	}
}
