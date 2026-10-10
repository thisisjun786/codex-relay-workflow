package dagsched

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1026 (d3): the bounded retry of not_proven_failing candidates on the surviving set, behind IntegrationBatchInput.RetryNotProven.
// The plan has four candidates in node-id order: a (a.txt), b (b.txt), c (repair or c.txt) and d (bad.txt). b and c are joined
// by an edge, so c depends on b. The merged tree fails when a and b coexist without repair.txt (the interaction), or when
// bad.txt is present (a genuinely failing candidate). The interaction is the only way the prefix [a, b] fails, and it is the
// case the bisection leaves out as not_proven_failing.

// repairFails is the verifier's predicate over the merged tree.
func repairFails(dir string) bool {
	has := func(name string) bool {
		_, err := os.Stat(filepath.Join(dir, name))
		return err == nil
	}
	return (has("a.txt") && has("b.txt") && !has("repair.txt")) || has("bad.txt")
}

// retryVerifier is the stub verifier with the predicate in front of it; runs counts every verification the batch starts.
func retryVerifier(t *testing.T, fails func(dir string) bool, runs *int) IntegrationBatchVerifier {
	t.Helper()
	inner := stubVerifier(writeStubVerifier(t))
	return func(ctx context.Context, dir string, env []string) error {
		*runs++
		if fails(dir) {
			return fmt.Errorf("the merged tree fails the predicate")
		}
		return inner(ctx, dir, env)
	}
}

// retryPlanKit is the four-candidate plan with an edge b -> c, declared in the plan's first revision so that c's reports
// and acceptances stand on the generation the edge makes; cFile is what c commits.
func retryPlanKit(t *testing.T, cFile string) *batchKit {
	t.Helper()
	return retryPlanKitEdges(t, cFile, [][2]string{{"b", "c"}})
}

// retryPlanKitEdges is the same plan with the given prerequisite edges (from, to) in its first revision.
func retryPlanKitEdges(t *testing.T, cFile string, edges [][2]string) *batchKit {
	t.Helper()
	nodes := []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{cFile: "c\n"}},
		{name: "d", files: map[string]string{"bad.txt": "bad\n"}},
	}
	k := &batchKit{releaseKit: newReleaseKit(t), repo: newGitRepo(t), heads: map[string]string{}}
	k.base = k.repo.git("rev-parse", "dev")
	var changes []doc
	for _, n := range nodes {
		k.repo.git("checkout", "-q", "-B", "feature-"+n.name, k.base)
		for path, body := range n.files {
			k.repo.write(path, body)
			k.repo.git("add", path)
		}
		k.repo.git("commit", "-q", "-m", "node "+n.name)
		k.heads[n.name] = k.repo.git("rev-parse", "HEAD")
		changes = append(changes, addRelNode(n.name, dag.NodeImplementation))
	}
	k.repo.git("checkout", "-q", "dev")
	for _, e := range edges {
		changes = append(changes, addEdge(e[0]+e[1], e[0], e[1], dag.EdgeArtifactVerified,
			doc{"pins_code_head": true, "target_repository": k.repo.path, "target_base_ref": "dev"}))
	}
	k.putPlan("g", 0, "g-r1", changes...)
	for _, n := range nodes {
		r := k.reportNode("g", n.name, acceptOpts{})
		k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
			r.Event, r.Acceptance.RelationshipID, 1, "verdict-turn", k.heads[n.name], "parent", k.clock())
	}
	for _, n := range []string{"a", "b", "c", "d"} {
		k.acceptByCommit(n)
	}
	return k
}

// runRetryBatch runs one batch of the kit with the retry flag set as asked, counting the verifications it starts.
func (k *batchKit) runRetryBatch(t *testing.T, retry bool, runs *int) (IntegrationBatchResult, error) {
	t.Helper()
	return k.runRetryBatchWith(t, retry, repairFails, runs)
}

// runRetryBatchWith is runRetryBatch with another predicate for the verifier.
func (k *batchKit) runRetryBatchWith(t *testing.T, retry bool, fails func(dir string) bool, runs *int) (IntegrationBatchResult, error) {
	t.Helper()
	in := k.batchIn()
	in.RetryNotProven = retry
	return k.sched.IntegrateBatch(context.Background(), in, IntegrationBatchDeps{Verify: retryVerifier(t, fails, runs), Update: updateIntegrationRef})
}

// splitReasons maps each split node to its reason.
func splitReasons(res IntegrationBatchResult) map[string]string {
	out := map[string]string{}
	for _, s := range res.Split {
		out[s.NodeID] = s.Reason
	}
	return out
}

// mergedNodes is the sorted node ids the batch merged.
func mergedNodes(res IntegrationBatchResult) []string {
	var out []string
	for _, m := range res.Merged {
		out = append(out, m.NodeID)
	}
	sort.Strings(out)
	return out
}

// Guard: the flag is off by default, so the split reasons stay as they were: b is not_proven_failing, c waits on b, d fails alone.
func TestIntegrationBatchRetryIsOffByDefault(t *testing.T) {
	k := retryPlanKit(t, "repair.txt")
	var runs int
	res, err := k.runRetryBatch(t, false, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); len(got) != 1 || got[0] != "a" {
		t.Fatalf("merged %v; want a alone with the flag off", got)
	}
	want := map[string]string{"b": "not_proven_failing", "c": "depends_on_b", "d": "verification_failed"}
	got := splitReasons(res)
	for n, r := range want {
		if got[n] != r {
			t.Fatalf("split reasons %v; want %v", got, want)
		}
	}
}

// (a) A later candidate that repairs the interaction of a prefix is dropped by the bisection today. With the retry on, the
// candidate that the prefix left out and the repairing candidate that depends on it merge together after the survivors.
func TestIntegrationBatchRetryMergesTheRepairAfterTheSurvivors(t *testing.T) {
	k := retryPlanKit(t, "repair.txt")
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("merged %v; want a, b and c", got)
	}
	if got := splitReasons(res); len(got) != 1 || got["d"] != "verification_failed" {
		t.Fatalf("split %v; want only d with verification_failed", got)
	}
	tree := k.repo.git("ls-tree", "-r", "--name-only", res.NewHead)
	if !strings.Contains(tree, "repair.txt") || !strings.Contains(tree, "b.txt") || strings.Contains(tree, "bad.txt") {
		t.Fatalf("the branch holds %q; want b.txt and repair.txt without bad.txt", tree)
	}
}

// (b) A candidate that really fails with the survivors stays out: the retry set fails as a whole, and the branch keeps the
// verified survivors only.
func TestIntegrationBatchRetryKeepsAFailingCandidateOut(t *testing.T) {
	k := retryPlanKit(t, "c.txt")
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a" {
		t.Fatalf("merged %v; want a alone", got)
	}
	want := map[string]string{"b": "not_proven_failing", "c": "depends_on_b", "d": "verification_failed"}
	got := splitReasons(res)
	for n, r := range want {
		if got[n] != r {
			t.Fatalf("split reasons %v; want %v", got, want)
		}
	}
	tree := k.repo.git("ls-tree", "-r", "--name-only", res.NewHead)
	if strings.Contains(tree, "b.txt") || strings.Contains(tree, "c.txt") || strings.Contains(tree, "bad.txt") || !strings.Contains(tree, "a.txt") {
		t.Fatalf("the branch holds %q; want a.txt only", tree)
	}
}

// (c) Only a PASS is ever issued: when the retry fails, the branch moves to the verified survivors and the record of that
// head passes.
func TestIntegrationBatchRetryIssuesOnlyAPass(t *testing.T) {
	k := retryPlanKit(t, "c.txt")
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	tip, found := k.branchTip("dev-int")
	if !found || tip != res.NewHead {
		t.Fatalf("the branch is %s (found %v); want the new head %s", tip, found, res.NewHead)
	}
	if res.Verification.Result != "pass" {
		t.Fatalf("the issued head carries a %q record; want pass", res.Verification.Result)
	}
	if res.Verification.HeadCommit != tip {
		t.Fatalf("the record names %s; the branch is at %s", res.Verification.HeadCommit, tip)
	}
}

// A failing retry returns to the verified survivors without making them again: the verifier of the failing retry set takes more
// than a second (the merge commits carry the time they are made), and the branch, the issued record and the merge commit of
// each merged candidate still name the one commit the survivors were verified at.
func TestIntegrationBatchRetryFailureKeepsTheVerifiedSurvivorCommits(t *testing.T) {
	k := retryPlanKit(t, "c.txt")
	var runs int
	slow := func(dir string) bool {
		bad := repairFails(dir)
		if bad {
			if _, err := os.Stat(filepath.Join(dir, "c.txt")); err == nil {
				time.Sleep(1100 * time.Millisecond)
			}
		}
		return bad
	}
	res, err := k.runRetryBatchWith(t, true, slow, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 1 || res.Merged[0].NodeID != "a" {
		t.Fatalf("merged %+v; want a alone", res.Merged)
	}
	tip, _ := k.branchTip("dev-int")
	if tip != res.NewHead || res.Verification.HeadCommit != tip || res.Merged[0].MergeCommit != tip {
		t.Fatalf("branch %s, new head %s, record %s, merge commit of a %s; want one commit", tip, res.NewHead, res.Verification.HeadCommit, res.Merged[0].MergeCommit)
	}
}

// The prerequisites of a retried candidate are judged on the current plan edges, all of them: c waits on b (not proven failing)
// and on d (failing alone), so it is not retried although its stored reason names b; only d's recovery would not matter either.
func TestIntegrationBatchRetryNeedsEveryPrerequisiteOfACandidate(t *testing.T) {
	k := retryPlanKitEdges(t, "repair.txt", [][2]string{{"b", "c"}, {"d", "c"}})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	for _, m := range res.Merged {
		if m.NodeID == "c" {
			t.Fatalf("merged %v; c must not merge while its prerequisite d is excluded", mergedNodes(res))
		}
	}
	if got := splitReasons(res); got["d"] != "verification_failed" || got["c"] == "" {
		t.Fatalf("split %v; want d verification_failed and c left out", got)
	}
}

// (d) The retry costs exactly one more full verification: the same plan with the flag on starts one verification more than
// with the flag off, in the case that merges the repair and in the case that reverts it.
func TestIntegrationBatchRetryIsExactlyOneExtraFullVerification(t *testing.T) {
	for _, cFile := range []string{"repair.txt", "c.txt"} {
		var off, on int
		if _, err := retryPlanKit(t, cFile).runRetryBatch(t, false, &off); err != nil {
			t.Fatalf("%s flag off: %v", cFile, err)
		}
		if _, err := retryPlanKit(t, cFile).runRetryBatch(t, true, &on); err != nil {
			t.Fatalf("%s flag on: %v", cFile, err)
		}
		if on-off != 1 {
			t.Fatalf("%s: the retry started %d verifications more than the batch without it; want exactly 1", cFile, on-off)
		}
	}
}

// (e) Replay is idempotent: the same batch with the flag on, run again after it moved the branch, merges nothing new, moves
// nothing, and every merged candidate keeps the one mark event its first run wrote. The batch identity carries the flag, so a
// replay with another setting is another batch. (A re-run writes one marked row per batch for the contained candidates; that
// is the existing contained path, and the event stays the same.)
func TestIntegrationBatchRetryReplayIsIdempotent(t *testing.T) {
	k := retryPlanKit(t, "repair.txt")
	var runs int
	first, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	eventsOf := func() map[string]map[string]bool {
		out := map[string]map[string]bool{}
		for _, r := range k.stageRows() {
			if r.Stage == "marked" {
				if out[r.NodeID] == nil {
					out[r.NodeID] = map[string]bool{}
				}
				out[r.NodeID][r.Detail] = true
			}
		}
		return out
	}
	before := eventsOf()
	if len(before) != 3 {
		t.Fatalf("marks after the first run cover %v; want a, b and c", before)
	}
	second, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if len(second.Merged) != 0 {
		t.Fatalf("the replay merged %+v; want nothing new", second.Merged)
	}
	if tip, _ := k.branchTip("dev-int"); tip != first.NewHead {
		t.Fatalf("the replay moved the branch to %s; want %s", tip, first.NewHead)
	}
	after := eventsOf()
	for node, events := range before {
		if len(events) != 1 {
			t.Fatalf("node %s has %d mark events after the first run; want one", node, len(events))
		}
		if !events[firstKey(after[node])] || len(after[node]) != 1 {
			t.Fatalf("node %s carries events %v after the replay; want only %v", node, after[node], events)
		}
	}
	in := k.batchIn()
	off := integrationBatchID(in, zeroObjectID, nil)
	in.RetryNotProven = true
	on := integrationBatchID(in, zeroObjectID, nil)
	if off == on {
		t.Fatal("the retry flag is not part of the batch identity")
	}
}

// firstKey is the one key of a single-event set, or "" when the set is empty.
func firstKey(set map[string]bool) string {
	for k := range set {
		return k
	}
	return ""
}
