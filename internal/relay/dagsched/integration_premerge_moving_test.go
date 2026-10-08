package dagsched

import (
	"context"
	"testing"
)

// CRW-952 c4: a candidate whose criteria change between its selection and the branch move is checked again inside the
// moving transaction, and the batch moves nothing. The change is made by the test hook that runs in the verification step.

func TestPremergeMovingTransactionRefusesACandidateChangedAfterSelection(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	stub := stubVerifier(writeStubVerifier(t))
	changed := false
	verify := func(ctx context.Context, dir string, env []string) error {
		if !changed {
			changed = true
			k.invRevise("g", "a", "g-r2", func(n doc) { n["criteria_set_digest"] = dig("changed in the batch") })
		}
		return stub(ctx, dir, env)
	}
	_, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: verify, Update: updateIntegrationRef})
	if got := refusalReasonOf(err); got != "merge_candidate_moved" {
		t.Fatalf("the moving transaction must refuse the changed candidate: reason %q (err %v)", got, err)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("a refused batch moved the integration branch")
	}
}
