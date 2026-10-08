package dagsched

import (
	"context"
	"strings"
	"testing"
)

// CRW-952 c4 (answer 3): integration judges the stored record of each candidate. A candidate with no record is left out
// of the batch with its premerge_* reason and never merged, and naming it with --node refuses with the same name.

// premergeIntDropRecord removes the stored record of node's acceptance, the shape an acceptance taken before the gate has.
func premergeIntDropRecord(k *batchKit, node string) {
	k.t.Helper()
	k.exec("DROP TRIGGER dag_acceptance_premerge_no_delete")
	k.exec("DELETE FROM dag_acceptance_premerge WHERE acceptance_id IN (SELECT acceptance_id FROM dag_acceptances WHERE node_id = ?)", node)
}

// TestPremergeIntegrationMergesTheJudgedAndHoldsTheRest: a plan with a candidate that has a passing record and one that has
// none merges the first, reports the second in split with premerge_missing, and moves the branch for the first only.
func TestPremergeIntegrationMergesTheJudgedAndHoldsTheRest(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	premergeIntDropRecord(k, "b")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0].NodeID != "a" {
		t.Fatalf("merged %+v; want only a", res.Merged)
	}
	if len(res.Split) != 1 || res.Split[0].NodeID != "b" || res.Split[0].Reason != "premerge_missing" {
		t.Fatalf("split %+v; want b with premerge_missing", res.Split)
	}
	tip, found := k.branchTip("dev-int")
	if !found || tip != res.NewHead {
		t.Fatalf("the integration branch is %s (found %v); want %s", tip, found, res.NewHead)
	}
	files := k.repo.git("ls-tree", "-r", "--name-only", tip)
	if !strings.Contains(files, "a.txt") || strings.Contains(files, "b.txt") {
		t.Fatalf("the integration branch holds %q; want a.txt and not b.txt", files)
	}
}

// TestPremergeIntegrationNamedCandidateWithoutRecordRefuses: dag-integrate --node names the held candidate, which refuses
// with its premerge_* name and merges nothing.
func TestPremergeIntegrationNamedCandidateWithoutRecordRefuses(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	premergeIntDropRecord(k, "b")
	in := k.batchIn()
	in.Nodes = []string{"b"}
	_, err := k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if got := refusalReasonOf(err); got != "premerge_missing" {
		t.Fatalf("a named candidate without its record refuses with premerge_missing: got %q (%v)", got, err)
	}
	if _, found := k.branchTip("dev-int"); found {
		t.Fatal("a refused batch must not create the integration branch")
	}
}
