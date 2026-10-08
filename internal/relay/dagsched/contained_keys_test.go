package dagsched

import (
	"context"
	"encoding/json"
	"testing"
)

// CRW-965 (parent decision, D2): a batch with only contained candidates runs the verifier exactly when the keys of the
// covering record no longer hold for the branch head's tree.
func TestAllContainedBatchVerifiesOnlyWhenTheCoveringKeysChanged(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	first := k.batchIn()
	first.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(ctx, first, counting); err != nil {
		t.Fatalf("first batch: %v", err)
	}
	if _, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting); err != nil {
		t.Fatalf("all-contained batch with unchanged keys: %v", err)
	}
	if runs != 1 {
		t.Fatalf("verifier ran %d times; an all-contained batch with unchanged keys must not verify (want 1)", runs)
	}
}

// CRW-965 (parent decision, D2): when the covering record's keys do not hold for the branch head's tree, the batch verifies
// the head without a merge, and the mark is written only after that PASS.
func TestAllContainedBatchReverifiesWhenTheCoveringKeysNoLongerHold(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	ctx := context.Background()
	k.repo.git("update-ref", "refs/heads/dev-int", k.heads["a"])
	detail, err := json.Marshal(map[string]string{"verified_head": k.heads["a"], "integration_ref": "dev-int", "verification_digest": "sha256:old",
		"tree": "stale-tree", "ci_digest": "sha256:stale", "dependency_go_sum": "", "dependency_web_lock": ""})
	if err != nil {
		t.Fatal(err)
	}
	if err := k.sched.stageRow(ctx, k.batchIn(), "earlier-batch", "intent", Candidate{}, string(detail)); err != nil {
		t.Fatal(err)
	}
	runs := 0
	base := stubVerifier(writeStubVerifier(t))
	counting := IntegrationBatchDeps{Verify: func(ctx context.Context, dir string, env []string) error {
		runs++
		return base(ctx, dir, env)
	}, Update: updateIntegrationRef}
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), counting)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if runs != 1 {
		t.Fatalf("verifier ran %d times; stale keys must be verified again (want 1)", runs)
	}
	if len(res.AlreadyContained) != 1 || res.AlreadyContained[0].MarkedEvent == "" {
		t.Fatalf("want a marked contained candidate after the PASS: %+v", res.AlreadyContained)
	}
	if tip, _ := k.branchTip("dev-int"); tip != k.heads["a"] {
		t.Fatalf("the branch moved to %s; a contained-only batch moves no ref", tip)
	}
}
