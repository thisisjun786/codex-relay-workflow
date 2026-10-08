package dagsched

import (
	"context"
	"testing"
)

// CRW-965 (review): a retried all-contained batch with the same identity reports the marks it already recorded and inserts
// no stage row twice.
func TestRetriedContainedBatchDoesNotInsertItsMarkTwice(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	deps := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}
	first := k.batchIn()
	first.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, first, deps); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), deps)
	if err != nil {
		t.Fatalf("contained batch: %v", err)
	}
	again, err := k.sched.IntegrateBatch(ctx, k.batchIn(), deps)
	if err != nil {
		t.Fatalf("retried contained batch: %v", err)
	}
	if again.BatchID != res.BatchID {
		t.Fatalf("the retry has another identity: %s and %s", again.BatchID, res.BatchID)
	}
	marked := 0
	for _, r := range k.stageRows() {
		if r.BatchID == res.BatchID && r.Stage == "marked" {
			marked++
		}
	}
	if marked > 1 {
		t.Fatalf("%d marked stage rows for one batch; the retry must not insert its mark again", marked)
	}
}

// CRW-965 (review): a batch for a checkout does not take a candidate accepted for another repository.
func TestBatchDoesNotTakeACandidateAcceptedForAnotherCheckout(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	other := newGitRepo(t)
	in := k.batchIn()
	in.Checkout = other.path
	if _, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err == nil {
		t.Fatal("a batch for another checkout must have no candidate to integrate")
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the candidate of another checkout moved a branch here")
	}
}
