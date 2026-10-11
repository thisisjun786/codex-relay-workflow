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
	return retryKitOf(t, []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{cFile: "c\n"}},
		{name: "d", files: map[string]string{"bad.txt": "bad\n"}},
	}, edges)
}

// retryKitOf is a plan of the given nodes, each accepted at the commit of its files, with the prerequisite edges (from, to)
// in its first revision.
func retryKitOf(t *testing.T, nodes []batchNode, edges [][2]string) *batchKit {
	t.Helper()
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
	for _, n := range nodes {
		k.acceptByCommit(n.name)
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

// When the bisection removes every candidate, the retry merges on the start, and each merged candidate records its own merge
// commit of the retry, an ancestor of the issued head, not a commit the settle loop made for a removed set: b is not proven
// failing, a and c depend on b, d fails alone, and a, b and c pass together.
func TestIntegrationBatchRetryOnNoSurvivorsRecordsTheRetriedMerges(t *testing.T) {
	k := retryPlanKitEdges(t, "repair.txt", [][2]string{{"b", "a"}, {"b", "c"}})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("merged %v (split %v); want a, b and c", got, splitReasons(res))
	}
	if res.Verification.HeadCommit != res.NewHead {
		t.Fatalf("the record names %s; the new head is %s", res.Verification.HeadCommit, res.NewHead)
	}
	for _, m := range res.Merged {
		ok, err := commitIsAncestor(context.Background(), k.repo.path, m.MergeCommit, res.NewHead)
		if err != nil || !ok {
			t.Errorf("%s records merge %s, not an ancestor of the issued head %s (err %v)", m.NodeID, m.MergeCommit, res.NewHead, err)
		}
		if subject := k.repo.git("show", "-s", "--format=%s", m.MergeCommit); subject != "CRW-965: integrate "+m.NodeID {
			t.Errorf("%s records the merge commit %q", m.NodeID, subject)
		}
	}
}

// A deferred candidate that the bisection cascades out is a prerequisite that stays out: d conflicts with a, so it is deferred,
// and it depends on b, so it is excluded when b is not proven failing; c depends on b and on d, so it is not retried although b is.
func TestIntegrationBatchRetryKeepsOutADependantOfAnExcludedDeferredCandidate(t *testing.T) {
	k := retryKitOf(t, []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n", "shared.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{"repair.txt": "c\n"}},
		{name: "d", files: map[string]string{"shared.txt": "d\n"}},
		{name: "e", files: map[string]string{"bad.txt": "e\n"}},
	}, [][2]string{{"b", "c"}, {"b", "d"}, {"d", "c"}})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	got := splitReasons(res)
	if got["d"] != "depends_on_b" {
		t.Fatalf("split %v; want d depends_on_b", got)
	}
	for _, m := range res.Merged {
		if m.NodeID == "c" {
			t.Fatalf("merged %v (split %v); c must not merge while its prerequisite d stays out", mergedNodes(res), got)
		}
	}
	if got["c"] == "" {
		t.Fatalf("split %v; want c left out", got)
	}
}

// A not_proven_failing candidate whose own prerequisite fails is not retried: b depends on d, b is removed first (node-id order)
// as not proven failing, then d fails alone; a, b and c would pass together, but b must not land without d.
func TestIntegrationBatchRetryKeepsOutANotProvenCandidateWhosePrerequisiteFails(t *testing.T) {
	k := retryPlanKitEdges(t, "repair.txt", [][2]string{{"d", "b"}})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,c" {
		t.Fatalf("merged %v (split %v); want a and c", got, splitReasons(res))
	}
	if got := splitReasons(res); got["d"] != "verification_failed" || got["b"] == "" {
		t.Fatalf("split %v; want d verification_failed and b left out", got)
	}
}

// deferredDependantKit is five candidates in node-id order, a to e, with the edge b -> e. d adds bad.txt and shared.txt, e adds
// shared.txt: e conflicts with d, so the first pass defers e; the bisection then removes b (not proven failing, it fails only
// beside a without repair.txt) and cascades e out as depends_on_b, and d fails alone. eFile is what e commits besides shared.txt.
func deferredDependantKit(t *testing.T, eFile string) *batchKit {
	t.Helper()
	return retryKitOf(t, []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{"repair.txt": "c\n"}},
		{name: "d", files: map[string]string{"bad.txt": "d\n", "shared.txt": "d\n"}},
		{name: "e", files: map[string]string{eFile: "e\n", "shared.txt": "e\n"}},
	}, [][2]string{{"b", "e"}})
}

// An eligible conflict-deferred dependant of a retried candidate joins the same retry (CRW-1026, d3 evaluation d1): e conflicted
// only with d, which fails alone, so once b recovers e merges on the survivors and b and e pass as one set.
func TestIntegrationBatchRetryIncludesAConflictDeferredDependant(t *testing.T) {
	k := deferredDependantKit(t, "e.txt")
	var off, on int
	if _, err := deferredDependantKit(t, "e.txt").runRetryBatch(t, false, &off); err != nil {
		t.Fatalf("flag off: %v", err)
	}
	res, err := k.runRetryBatch(t, true, &on)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,c,e" {
		t.Fatalf("merged %v (split %v); want a, b, c and e", got, splitReasons(res))
	}
	if got := splitReasons(res); len(got) != 1 || got["d"] != "verification_failed" {
		t.Fatalf("split %v; want only d with verification_failed", got)
	}
	if on-off != 1 {
		t.Fatalf("the retry started %d verifications more than the batch without it; want exactly 1", on-off)
	}
	tree := k.repo.git("ls-tree", "-r", "--name-only", res.NewHead)
	if !strings.Contains(tree, "e.txt") || !strings.Contains(tree, "b.txt") || strings.Contains(tree, "bad.txt") {
		t.Fatalf("the branch holds %q; want b.txt and e.txt without bad.txt", tree)
	}
	if res.Verification.HeadCommit != res.NewHead || res.Verification.Result != "pass" {
		t.Fatalf("the issued record names %s with %q; want a pass of %s", res.Verification.HeadCommit, res.Verification.Result, res.NewHead)
	}
}

// The deferred dependant can be the repair itself: b fails beside a until e brings repair.txt, so only the pair b and e passes.
func TestIntegrationBatchRetryMergesADeferredDependantThatRepairsTheRetriedCandidate(t *testing.T) {
	k := retryKitOf(t, []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{"c.txt": "c\n"}},
		{name: "d", files: map[string]string{"bad.txt": "d\n", "shared.txt": "d\n"}},
		{name: "e", files: map[string]string{"repair.txt": "e\n", "shared.txt": "e\n"}},
	}, [][2]string{{"b", "e"}})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,c,e" {
		t.Fatalf("merged %v (split %v); want a, b, c and e", got, splitReasons(res))
	}
}

// A deferred dependant whose conflict stays does not sink the retry: e conflicts with the survivor a, so it is left out and b
// is still retried and merged; e is reported with the reason it carried.
func TestIntegrationBatchRetryLeavesOutADeferredDependantThatStillConflicts(t *testing.T) {
	k := retryKitOf(t, []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n", "shared.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{"repair.txt": "c\n"}},
		{name: "d", files: map[string]string{"bad.txt": "d\n"}},
		{name: "e", files: map[string]string{"e.txt": "e\n", "shared.txt": "e\n"}},
	}, [][2]string{{"b", "e"}})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,c" {
		t.Fatalf("merged %v (split %v); want a, b and c", got, splitReasons(res))
	}
	if got := splitReasons(res); got["e"] == "" || got["d"] != "verification_failed" {
		t.Fatalf("split %v; want e left out and d verification_failed", got)
	}
}

// fallbackKit is six candidates in node-id order, a to f, with the edges b -> e and b -> f. c adds bad.txt and shared.txt and
// fails alone; d is the repair; e adds shared.txt, so the first pass defers e; the bisection removes b (not proven failing: it
// fails only beside a before the repair) and cascades e and f out as depends_on_b, then c. The survivors are a and d. f adds
// f.txt and other.txt; eFiles is what e commits besides shared.txt.
func fallbackKit(t *testing.T, eFiles map[string]string) *batchKit {
	t.Helper()
	e := map[string]string{"shared.txt": "e\n"}
	for path, body := range eFiles {
		e[path] = body
	}
	return retryKitOf(t, []batchNode{
		{name: "a", files: map[string]string{"a.txt": "a\n"}},
		{name: "b", files: map[string]string{"b.txt": "b\n"}},
		{name: "c", files: map[string]string{"bad.txt": "c\n", "shared.txt": "c\n"}},
		{name: "d", files: map[string]string{"repair.txt": "d\n"}},
		{name: "e", files: e},
		{name: "f", files: map[string]string{"f.txt": "f\n", "other.txt": "f\n"}},
	}, [][2]string{{"b", "e"}, {"b", "f"}})
}

// treeFiles is the set of file paths in a commit's tree.
func treeFiles(k *batchKit, commit string) map[string]bool {
	out := map[string]bool{}
	for _, path := range strings.Split(k.repo.git("ls-tree", "-r", "--name-only", commit), "\n") {
		out[path] = true
	}
	return out
}

// A conflict-deferred candidate never costs the retry of the others: e merges in the retry set but f, a plain retried candidate,
// then conflicts with it (both add other.txt). The set is merged again without e, so b and f still recover beside the survivors
// a and d as they do when deferred candidates are not retried at all, and e stays out.
func TestIntegrationBatchRetryKeepsTheOthersWhenAPlainCandidateConflictsWithADeferredOne(t *testing.T) {
	k := fallbackKit(t, map[string]string{"e.txt": "e\n", "other.txt": "e\n"})
	var runs int
	res, err := k.runRetryBatch(t, true, &runs)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,d,f" {
		t.Fatalf("merged %v (split %v); want a, b, d and f", got, splitReasons(res))
	}
	if got := splitReasons(res); len(got) != 2 || got["c"] != "verification_failed" || got["e"] == "" {
		t.Fatalf("split %v; want c verification_failed and e left out", got)
	}
	tree := treeFiles(k, res.NewHead)
	if !tree["f.txt"] || tree["e.txt"] || tree["bad.txt"] {
		t.Fatalf("the branch holds %v; want f.txt without e.txt and bad.txt", tree)
	}
	if res.Verification.HeadCommit != res.NewHead || res.Verification.Result != "pass" {
		t.Fatalf("the issued record names %s with %q; want a pass of %s", res.Verification.HeadCommit, res.Verification.Result, res.NewHead)
	}
}

// A conflict-deferred candidate that breaks the retry set does not sink it either: e merges but brings worse.txt, so the set
// fails; the set without e is verified once more and b and f recover. That second verification is the only one the deferred
// candidates can add: two more than the batch without the retry.
func TestIntegrationBatchRetryKeepsTheOthersWhenADeferredCandidateFailsTheSet(t *testing.T) {
	fails := func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "worse.txt"))
		return repairFails(dir) || err == nil
	}
	eFiles := map[string]string{"worse.txt": "e\n"}
	var off, on int
	if _, err := fallbackKit(t, eFiles).runRetryBatchWith(t, false, fails, &off); err != nil {
		t.Fatalf("flag off: %v", err)
	}
	k := fallbackKit(t, eFiles)
	res, err := k.runRetryBatchWith(t, true, fails, &on)
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if got := mergedNodes(res); strings.Join(got, ",") != "a,b,d,f" {
		t.Fatalf("merged %v (split %v); want a, b, d and f", got, splitReasons(res))
	}
	if got := splitReasons(res); len(got) != 2 || got["c"] != "verification_failed" || got["e"] == "" {
		t.Fatalf("split %v; want c verification_failed and e left out", got)
	}
	if on-off != 2 {
		t.Fatalf("the retry started %d verifications more than the batch without it; want 2 (the set with e, then without it)", on-off)
	}
	tree := treeFiles(k, res.NewHead)
	if tree["worse.txt"] || !tree["f.txt"] {
		t.Fatalf("the branch holds %v; want f.txt without worse.txt", tree)
	}
	if res.Verification.HeadCommit != res.NewHead || res.Verification.Result != "pass" {
		t.Fatalf("the issued record names %s with %q; want a pass of %s", res.Verification.HeadCommit, res.Verification.Result, res.NewHead)
	}
}
