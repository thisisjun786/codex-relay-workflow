package dagsched

import (
	"context"
	"errors"
	"testing"
)

// CRW-965 (parent decision, D6): an epoch claimed by another coordinator after the verification and before the swap refuses
// the batch, and the branch does not move.
func TestIntegrationBatchRefusesAnEpochClaimedBeforeTheSwap(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	base := stubVerifier(writeStubVerifier(t))
	deps := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		if err := base(ctx, dir, env); err != nil {
			return err
		}
		k.session("g", "parent", "nonce-replacement")
		return nil
	}, Update: updateIntegrationRef}
	_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), deps)
	if !isStale(err) {
		t.Fatalf("a batch whose epoch was claimed before the swap must be refused as stale: %v", err)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("the branch moved although the batch's epoch was no longer current")
	}
}

// CRW-965 (parent decision, D6): a crash between the swap and the commit leaves the planned intent. The next run records the
// move once, without a second move, and marks the candidate the branch already holds.
func TestIntegrationBatchReconcilesAPlannedMoveAfterACrash(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	crashing := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: func(ctx context.Context, checkout, ref, newCommit, oldCommit string) error {
		if err := updateIntegrationRef(ctx, checkout, ref, newCommit, oldCommit); err != nil {
			return err
		}
		return errors.New("simulated crash between the swap and the commit")
	}}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), crashing); err == nil {
		t.Fatal("the crashing batch should fail after the swap")
	}
	moved, found := k.branchTip("dev-int")
	if !found {
		t.Fatal("the swap should have moved the branch")
	}
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
	if err != nil {
		t.Fatalf("next run: %v", err)
	}
	if len(res.Reconciled) != 1 {
		t.Fatalf("the next run should reconcile the planned move once: %+v", res.Reconciled)
	}
	if tip, _ := k.branchTip("dev-int"); tip != moved || runs != 0 {
		t.Fatalf("the branch moved to %s (was %s) and the verifier ran %d times; reconciliation makes no second move", tip, moved, runs)
	}
	if len(res.AlreadyContained) != 1 || res.AlreadyContained[0].MarkedEvent == "" {
		t.Fatalf("the reconciled candidate should be marked: %+v", res.AlreadyContained)
	}
}
