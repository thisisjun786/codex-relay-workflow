package dagsched

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// CRW-1033. A node accepted on its pull request carries the forge slug of that pull request as its repository, a node
// accepted by commit carries the checkout path. The batch takes a candidate whose repository is the checkout or the
// slug the checkout's origin remote names, and still judges a candidate of another slug elsewhere. The nodes are
// accepted through dag-accept's real pull-request and commit paths.

// wireForge points the kit at a forge: the checkout's origin remote names owner/repo on github.com, the forge slug
// owner/repo is the checkout for the tip and ancestry readers, and a pull request is read from the scripted forge.
func (k *batchKit) wireForge(t *testing.T) {
	t.Helper()
	t.Setenv("GH_HOST", "github.com")
	k.repo.git("remote", "add", "origin", "https://github.com/owner/repo.git")
	readers := newForgeKitReaders(t)
	readers.mapTo("owner/repo", k.repo.path)
	k.sched.Tips, k.sched.Ancestry = readers, readers.Ancestry
}

// acceptByPullRequest accepts a node on pull request number of the forge slug at the node's head, as dag-accept does without --commit.
func (k *batchKit) acceptByPullRequest(node, slug string, number int64) AcceptResult {
	k.t.Helper()
	key := fmt.Sprintf("%s#%d", slug, number)
	k.forge.by[key] = openPR(slug, number, k.heads[node])
	res, err := k.accept("g", node, AcceptInput{PullRequest: &PRRef{Repository: slug, Number: number}})
	if err != nil {
		k.t.Fatalf("accept %s on %s: %v", node, key, err)
	}
	return res
}

func (k *batchKit) integrateDeps(t *testing.T) IntegrationBatchDeps {
	return IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}
}

// (a) Nodes accepted only on pull requests of the checkout's own forge repository are candidates, and the batch merges, verifies and marks them.
func TestIntegrationBatchTakesNodesAcceptedOnPullRequests(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.wireForge(t)
	k.acceptByPullRequest("a", "owner/repo", 11)
	k.acceptByPullRequest("b", "owner/repo", 12)
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), k.integrateDeps(t))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 2 || len(res.Split) != 0 {
		t.Fatalf("merged %d split %d; want 2 and 0: %+v", len(res.Merged), len(res.Split), res)
	}
	tip, found := k.branchTip("dev-int")
	if !found || tip != res.NewHead {
		t.Fatalf("the integration branch is %s (found %v); want %s", tip, found, res.NewHead)
	}
	if files := k.repo.git("ls-tree", "-r", "--name-only", tip); !strings.Contains(files, "a.txt") || !strings.Contains(files, "b.txt") {
		t.Fatalf("the integration branch holds %q; want a.txt and b.txt", files)
	}
	if n := countStage(k.stageRows(), "marked"); n != 2 {
		t.Fatalf("%d merged marks written; want 2", n)
	}
}

// (a) A named pull-request node is found as well.
func TestIntegrationBatchFindsANamedPullRequestNode(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.wireForge(t)
	k.acceptByPullRequest("a", "owner/repo", 11)
	in := k.batchIn()
	in.Nodes = []string{"a"}
	res, err := k.sched.IntegrateBatch(context.Background(), in, k.integrateDeps(t))
	if err != nil || len(res.Merged) != 1 || res.Merged[0].NodeID != "a" {
		t.Fatalf("batch = %v %+v; want node a merged", err, res)
	}
}

// (b) Pull-request and commit acceptances in one plan are one batch.
func TestIntegrationBatchMergesPullRequestAndCommitNodesTogether(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.wireForge(t)
	k.acceptByPullRequest("a", "owner/repo", 11)
	k.acceptByCommit("b")
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), k.integrateDeps(t))
	if err != nil {
		t.Fatalf("batch: %v", err)
	}
	if len(res.Merged) != 2 || len(res.Split) != 0 {
		t.Fatalf("merged %d split %d; want 2 and 0: %+v", len(res.Merged), len(res.Split), res)
	}
	if n := countStage(k.stageRows(), "marked"); n != 2 {
		t.Fatalf("%d merged marks written; want 2", n)
	}
	if len(res.Targets) != 1 {
		t.Fatalf("targets %v; want the one integration ref", res.Targets)
	}
}

// (c) A pull request of another repository's slug is judged there, as before, whether the batch names it or not.
func TestIntegrationBatchRefusesAPullRequestOfAnotherSlug(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}}, batchNode{name: "b", files: map[string]string{"b.txt": "b\n"}})
	k.wireForge(t)
	k.sched.Tips.(*forgeKitReaders).mapTo("other/repo", k.repo.path)
	k.acceptByPullRequest("a", "other/repo", 11)
	k.acceptByCommit("b")
	in := k.batchIn()
	in.Nodes = []string{"a"}
	if _, err := k.sched.IntegrateBatch(context.Background(), in, k.integrateDeps(t)); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "not a ready accepted candidate") {
		t.Fatalf("named batch = %v; want the candidate of another repository refused", err)
	}
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), k.integrateDeps(t))
	if err != nil || len(res.Merged) != 1 || res.Merged[0].NodeID != "b" {
		t.Fatalf("batch = %v %+v; want only the commit node b merged", err, res)
	}
}

// (c) A checkout without an origin remote names no slug, so a pull-request node is not its candidate.
func TestIntegrationBatchWithoutAnOriginTakesNoPullRequestNode(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.wireForge(t)
	k.repo.git("remote", "remove", "origin")
	k.acceptByPullRequest("a", "owner/repo", 11)
	if _, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), k.integrateDeps(t)); refusalReasonOf(err) != "disposition_conflict" {
		t.Fatalf("batch = %v; want disposition_conflict with no candidate", err)
	}
}

// (d) A pull-request node whose head the parent refreshed onto the base (dag-base-refresh) is merged at the head it stands on now, not at the head it was accepted at.
func TestIntegrationBatchMergesARefreshedPullRequestNodeAtItsStandHead(t *testing.T) {
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.wireForge(t)
	k.acceptByPullRequest("a", "owner/repo", 11)
	accepted := k.heads["a"]
	// dev moves on and the branch of the pull request takes one clean merge of it
	k.repo.commit("other.txt", "dev one\n")
	k.repo.git("checkout", "-q", "feature-a")
	k.repo.git("merge", "-q", "--no-ff", "-m", "merge dev", "dev")
	refreshed := k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	k.forge.by["owner/repo#11"] = openPR("owner/repo", 11, refreshed)
	if rec, err := k.sched.RecordBaseRefresh(context.Background(), "g", "a", "parent", RefreshInput{Checkout: k.repo.path}); err != nil || rec.HeadSHA != refreshed {
		t.Fatalf("record base refresh = %v %+v", err, rec)
	}
	// before the stand head has its own pre-merge record the batch leaves the node out: the record of the accepted head does not pass for the head it stands on now
	if res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), k.integrateDeps(t)); err == nil || len(res.Merged) != 0 {
		t.Fatalf("batch before the stand head's record = %v %+v; want node a left out", err, res)
	}
	// the refreshed head is judged on its own pre-merge record (CRW-952), given through dag-accept on the same pull request: a record of another head is refused, the
	// record of the stand head is stored as the record the acceptance now stands on, and the same call again is refused as a second record of that head
	pull := &PRRef{Repository: "owner/repo", Number: 11}
	if _, err := k.accept("g", "a", AcceptInput{PullRequest: pull, Premerge: premergeAt(k.sched, context.Background(), "g", "a", accepted)}); refusalReasonOf(err) != "premerge_head_mismatch" {
		t.Fatalf("dag-accept with the record of the accepted head = %v; want premerge_head_mismatch", err)
	}
	var event, criteria string
	if err := k.s.DB.QueryRow("SELECT a.event_id, a.criteria_set_digest FROM dag_acceptances a WHERE a.plan_id = 'g' AND a.node_id = 'a' AND a.state = 'active'").Scan(&event, &criteria); err != nil {
		t.Fatal(err)
	}
	restood, err := k.accept("g", "a", AcceptInput{PullRequest: pull, Premerge: premergeAt(k.sched, context.Background(), "g", "a", refreshed)})
	if err != nil || !restood.Replayed || restood.SupersededID != "" {
		t.Fatalf("dag-accept with the record of the stand head = %v %+v; want the same acceptance with the stand head's record", err, restood)
	}
	if _, err := k.accept("g", "a", AcceptInput{PullRequest: pull, Premerge: premergeAt(k.sched, context.Background(), "g", "a", refreshed)}); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "already holds") {
		t.Fatalf("a second record of the stand head = %v; want disposition_conflict", err)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_acceptances WHERE plan_id = 'g' AND node_id = 'a' AND state = 'active' AND criteria_set_digest = ?", criteria); got != 1 {
		t.Fatalf("%d active acceptances of a with its criteria; want the one acceptance", got)
	}
	res, err := k.sched.IntegrateBatch(context.Background(), k.batchIn(), k.integrateDeps(t))
	if err != nil || len(res.Merged) != 1 || res.Merged[0].HeadSHA != refreshed || res.Merged[0].HeadSHA == accepted {
		t.Fatalf("batch = %v %+v; want node a merged at the refreshed head %s (accepted %s)", err, res, refreshed, accepted)
	}
	if n := countStage(k.stageRows(), "marked"); n != 1 {
		t.Fatalf("%d merged marks written; want 1", n)
	}
	// the observation that follows the batch ties the refreshed head and the merged mark of the acceptance together: the node is integrated at the head it stands on
	var revision string
	if err := k.s.DB.QueryRow("SELECT a.revision_hash FROM dag_acceptances a WHERE a.plan_id = 'g' AND a.node_id = 'a' AND a.state = 'active'").Scan(&revision); err != nil {
		t.Fatal(err)
	}
	var marks int
	if err := k.s.DB.QueryRow("SELECT COUNT(*) FROM assignment_marks WHERE mark = 'merged' AND event_id = ? AND revision_hash = ?", event, revision).Scan(&marks); err != nil || marks != 1 {
		t.Fatalf("%d merged marks on the acceptance's event and revision (%v); want 1", marks, err)
	}
	observed, err := k.sched.ObserveIntegration(context.Background(), "g", "a", "parent", []Target{{Repository: "owner/repo", BaseRef: "dev-int"}})
	if err != nil || !observed.Integrated || len(observed.Observations) != 1 || observed.Observations[0].SubjectSHA != refreshed || !observed.Observations[0].IsAncestor {
		t.Fatalf("observation after the batch = %v %+v; want the refreshed head %s integrated on dev-int", err, observed, refreshed)
	}
}
