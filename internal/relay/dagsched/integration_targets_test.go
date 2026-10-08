package dagsched

import (
	"context"
	"errors"
	"testing"
)

// CRW-965 (parent decision D4): only an integration ref a batch has moved is a completion target. A batch that failed
// before its move, on another ref, adds nothing to the refs an acceptance completes on.
func TestIntegrationTargetsCountOnlyMovedRefs(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	acc, found, err := loadActiveAcceptance(ctx, k.sched.Store.Q(ctx), "g", "a")
	if err != nil || !found {
		t.Fatalf("the active acceptance of a: %v, %v", found, err)
	}
	failing := k.batchIn()
	failing.IntegrationRef = "dev-failed"
	crash := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(context.Context, string, string, string, string) error {
		return errors.New("simulated refusal before the branch moved")
	}}
	if _, err := k.sched.IntegrateBatch(ctx, failing, crash); err == nil {
		t.Fatal("the failing batch should not complete")
	}
	if _, found := k.branchTip("dev-failed"); found {
		t.Fatal("the failing batch must not have moved dev-failed")
	}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}); err != nil {
		t.Fatalf("the moving batch: %v", err)
	}
	refs, err := integrationRefsOf(ctx, k.sched.Store.Q(ctx), acc.AcceptanceID)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0] != "dev-int" {
		t.Fatalf("the completion targets are the moved ref only: %v", refs)
	}
}
