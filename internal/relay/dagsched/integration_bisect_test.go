package dagsched

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// writePairVerifier is a verify command that exits 1 only when both x.txt and y.txt are present in the worktree it runs in,
// so a failure needs two candidates together.
func writePairVerifier(t *testing.T) string {
	t.Helper()
	stub := writeStubVerifier(t)
	script := filepath.Join(t.TempDir(), "verify-pair.sh")
	body := "#!/bin/sh\nif [ -e x.txt ] && [ -e y.txt ]; then\n  echo 'x.txt and y.txt together fail' >&2\n  exit 1\nfi\nexec " + stub + "\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return script
}

// CRW-965 (parent decision D5): a candidate the bisection leaves out only because it breaks a prefix is reported as not
// proven failing, never as failed; it gets no mark and stays ready, and the next batch integrates it.
func TestIntegrationBatchReportsACandidateThatOnlyBreaksTheSetAsNotProvenFailing(t *testing.T) {
	k := newBatchKit(t,
		batchNode{name: "a", files: map[string]string{"x.txt": "x\n"}},
		batchNode{name: "b", files: map[string]string{"y.txt": "y\n"}})
	k.acceptByCommit("a")
	k.acceptByCommit("b")
	ctx := context.Background()
	res, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writePairVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("the batch: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0].NodeID != "a" {
		t.Fatalf("a alone is the verified set: %+v", res.Merged)
	}
	var reasons []string
	for _, s := range res.Split {
		if s.NodeID == "b" {
			reasons = append(reasons, s.Reason)
		}
	}
	if len(reasons) != 1 || reasons[0] != "not_proven_failing" {
		t.Fatalf("b breaks the set but fails alone nowhere: its split reason must be not_proven_failing, got %v", reasons)
	}
	for _, m := range k.stageRows() {
		if m.Stage == "marked" && m.NodeID == "b" {
			t.Fatal("a candidate left out by the bisection must get no mark")
		}
	}
	if _, ok := k.branchTip("dev-int"); !ok {
		t.Fatal("the branch should hold the verified set")
	}
	// the next batch verifies the set without the pair rule (the rule has since been dropped), so b integrates
	next, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil {
		t.Fatalf("the next batch: %v", err)
	}
	if len(next.Merged) != 1 || next.Merged[0].NodeID != "b" {
		t.Fatalf("the next batch integrates b, which stayed ready: %+v", next.Merged)
	}
}
