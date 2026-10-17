package dagsched

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// TestSchedRelayMilestoneOrderMatchesReady (CRW-1277 c, the scheduler's half): OfferingOrder, the order the run's milestone lists its
// nodes in, is the order Ready offers the nodes that are ready together: the longest chain below a node first, then the most nodes
// below it, then the node id; the run's own test (internal/flow/cli) reads a milestone through it.
func TestSchedRelayMilestoneOrderMatchesReady(t *testing.T) {
	t.Parallel()
	f := newFixture(t)
	f.projectParent()
	// R1, R2 and R3 are ready together; two nodes hang below R2 in a chain, one below R3, none below R1
	f.putPlan("order", 0, "order-r1", addRelNode("R1", dag.NodeNonPR), addRelNode("R2", dag.NodeNonPR), addRelNode("R3", dag.NodeNonPR),
		addRelNode("S1", dag.NodeNonPR), addRelNode("S2", dag.NodeNonPR), addRelNode("T1", dag.NodeNonPR),
		addEdge("r2s1", "R2", "S1", dag.EdgeArtifactVerified, nil), addEdge("s1s2", "S1", "S2", dag.EdgeArtifactVerified, nil), addEdge("r3t1", "R3", "T1", dag.EdgeArtifactVerified, nil))
	order := OfferingOrder(f.snapshot("order"))
	if want := []string{"R2", "R3", "S1", "R1", "S2", "T1"}; !slices.Equal(order, want) {
		t.Fatalf("the offering order is %v, want %v", order, want)
	}
	reading, err := f.sched.Read(context.Background(), "order", ReadyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var ready []string
	for _, n := range reading.Ready {
		ready = append(ready, n.NodeID)
	}
	var offered []string
	for _, id := range order {
		if slices.Contains(ready, id) {
			offered = append(offered, id)
		}
	}
	if len(ready) != 3 || !slices.Equal(ready, offered) {
		t.Fatalf("Ready offers %v, the offering order lists them as %v (%s)", ready, offered, reading.brief())
	}
}

// CRW-1595 (finding 1): the ref updater itself refuses to move a branch a worktree has checked out — a caller that
// does not go through the batch gets the same protection. An unchecked branch still moves by the compare-and-swap,
// and the swap still fails when the old head differs.
func TestCRW1595UpdateIntegrationRefRefusesACheckedOutBranch(t *testing.T) {
	repo := newGitRepo(t)
	base := repo.git("rev-parse", "dev")
	repo.git("branch", "dev-int", base)
	linked := filepath.Join(t.TempDir(), "linked wt\nint")
	repo.git("worktree", "add", linked, "dev-int")
	t.Cleanup(func() { repo.git("worktree", "remove", "--force", linked) })
	err := UpdateIntegrationRef(context.Background(), repo.path, "dev-int", base, base)
	if err == nil || !strings.Contains(err.Error(), "checked out") {
		t.Fatalf("moving a checked-out branch: %v", err)
	}
	if refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("reason %q, want disposition_conflict", refusalReasonOf(err))
	}
	if tip := repo.git("rev-parse", "dev-int"); tip != base {
		t.Fatalf("the checked-out branch moved to %s", tip)
	}
	// an unchecked branch moves by the compare-and-swap as before
	repo.git("branch", "other", base)
	moved := repo.commit("other.txt", "other\n")
	if err := UpdateIntegrationRef(context.Background(), repo.path, "other", moved, base); err != nil {
		t.Fatalf("the CAS of an unchecked branch: %v", err)
	}
	if tip := repo.git("rev-parse", "other"); tip != moved {
		t.Fatalf("other is %s, want %s", tip, moved)
	}
	// and it still fails when the expected head differs
	if err := UpdateIntegrationRef(context.Background(), repo.path, "other", base, base); err == nil {
		t.Fatal("the CAS accepted a wrong old head")
	}
	if tip := repo.git("rev-parse", "other"); tip != moved {
		t.Fatalf("the failed CAS moved other to %s", tip)
	}
}

// CRW-1595 (finding 2): a failing verification command's error carries the bounded tail of BOTH its streams, valid
// UTF-8 with a marker when output was clipped; a short successful command is still nil.
func TestCRW1595CommandVerifierReportsBothStreams(t *testing.T) {
	dir := t.TempDir()
	fail := filepath.Join(dir, "fail.sh")
	writeExecutable(t, fail, "#!/bin/sh\necho 'stdout: --- FAIL: TestCRW1595Verifier'\necho 'stderr: the suite failed' >&2\nexit 1\n")
	err := CommandVerifierEnv([]string{fail}, func() []string { return nil })(context.Background(), dir, nil)
	if err == nil {
		t.Fatal("a nonzero exit answered nil")
	}
	for _, want := range []string{"--- FAIL: TestCRW1595Verifier", "the suite failed"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the failure lacks %q (only one stream was kept): %v", want, err)
		}
	}
	if !utf8.ValidString(err.Error()) {
		t.Errorf("the failure is not valid UTF-8")
	}
	loud := filepath.Join(dir, "loud.sh")
	writeExecutable(t, loud, "#!/bin/sh\ni=0\nwhile [ $i -lt 400 ]; do echo 'a very loud failure line that fills the diagnostic tail quickly'; i=$((i+1)); done\nexit 1\n")
	err = CommandVerifierEnv([]string{loud}, func() []string { return nil })(context.Background(), dir, nil)
	if err == nil {
		t.Fatal("a loud nonzero exit answered nil")
	}
	if len(err.Error()) > 8192+256 {
		t.Errorf("the failure is %d bytes, over the bound", len(err.Error()))
	}
	if !strings.Contains(err.Error(), "[earlier output omitted]") || !utf8.ValidString(err.Error()) {
		t.Errorf("the clipped failure is unmarked or invalid: %q", err.Error())
	}
	ok := filepath.Join(dir, "ok.sh")
	writeExecutable(t, ok, "#!/bin/sh\nexit 0\n")
	if err := CommandVerifierEnv([]string{ok}, func() []string { return nil })(context.Background(), dir, nil); err != nil {
		t.Fatalf("a successful command: %v", err)
	}
}
