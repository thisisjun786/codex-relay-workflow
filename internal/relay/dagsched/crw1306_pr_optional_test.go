package dagsched

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1306: an implementation node is accepted with a local verification-record/1 only; a pull request is optional and
// GitHub is plain external storage, not a gate. Every step after the acceptance that branches on the forge row
// (dag_acceptance_forge) proceeds for a commit acceptance as it does for a pull-request one. The nodes are accepted
// through dag-accept's real commit and pull-request paths, integrated by the real batch, and pushed to a temporary
// bare remote under a temporary HOME.

// noForge is a pull request reader that fails the test when it is asked: a commit acceptance reads no forge.
func noForge(t *testing.T) PullRequestReader {
	return func(_ context.Context, repository string, number int64) (PullRequest, error) {
		t.Errorf("the forge was read for %s#%d on the commit path", repository, number)
		return PullRequest{}, errors.New("no forge on the commit path")
	}
}

// A node accepted by its commit (no pull request, no forge row) passes the merge check, is integrated by dag-integrate,
// pushed to the remote by dag-integrate-push, observed on the integration branch and counted as integrated by progress.
func TestCommitAcceptedNodePassesTheMergeCheckAndDagIntegrate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	remote := bareRemote(t)
	k.pushRepo(t, remote)
	k.sched.Tips, k.sched.Ancestry, k.sched.PRs = newLegacyLocalTipReader(t), GitAncestry{}.Ancestry, noForge(t)
	k.acceptByCommit("a")
	if n := k.count("SELECT COUNT(*) FROM dag_acceptance_forge"); n != 0 {
		t.Fatalf("the commit path wrote %d forge rows", n)
	}

	judged, err := k.sched.Judge(ctx, "g", "a", "parent", JudgeInput{})
	if err != nil {
		t.Fatalf("the merge check of a commit acceptance: %v", err)
	}
	if !judged.Eligible() || judged.HeadSHA != k.heads["a"] || judged.AcceptanceID == "" {
		t.Fatalf("the merge check = %+v; want eligible at the accepted head %s", judged, k.heads["a"])
	}
	if !strings.Contains(judged.Reason, "dag-integrate") {
		t.Fatalf("the merge check reason %q does not name the route the commit takes", judged.Reason)
	}
	// the merge lane carries pull requests only: a commit acceptance is sent to dag-integrate, never stalled on the forge row
	if _, _, err := k.sched.RequestMergeTurn(ctx, "g", "a", "parent", MergeRequestInput{Host: "host"}); err == nil ||
		!strings.Contains(err.Error(), "dag-integrate") || strings.Contains(err.Error(), "forge identity") {
		t.Fatalf("a merge turn for a commit acceptance = %v; want the dag-integrate route, not a missing forge row", err)
	}

	batch, err := k.sched.IntegrateBatch(ctx, k.batchIn(), IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef})
	if err != nil || len(batch.Merged) != 1 || batch.Merged[0].NodeID != "a" {
		t.Fatalf("dag-integrate = %+v, %v; want node a merged", batch, err)
	}
	pushed, err := PushIntegration(ctx, k.repo.path, "origin", "dev", "dev-int", k.sched.VerifiedMoveOnto, nil)
	if err != nil || pushed.Outcome != PushPushed {
		t.Fatalf("dag-integrate-push = %+v, %v; want pushed", pushed, err)
	}
	if got := remoteBranch(t, remote, "dev"); got != batch.NewHead {
		t.Fatalf("the remote dev is %s; want the integration commit %s", got, batch.NewHead)
	}
	// a terminal node names its target, as on the pull-request path: the integration ref dag-integrate reported
	if len(batch.Targets) != 1 {
		t.Fatalf("dag-integrate reported the targets %v; want the one integration ref", batch.Targets)
	}
	obs, err := k.sched.ObserveIntegration(ctx, "g", "a", "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}})
	if err != nil || !obs.Integrated {
		t.Fatalf("observe a on the integration branch its batch moved: %+v, %v; want integrated", obs, err)
	}
	progress, err := k.sched.ReadProgress(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Cumulative.Integrated.Nodes != 1 || progress.Blocked.Nodes != 0 {
		t.Fatalf("progress counts %d integrated and %d blocked; want 1 and 0", progress.Cumulative.Integrated.Nodes, progress.Blocked.Nodes)
	}
	for _, n := range progress.Nodes {
		if n.NodeID == "a" && n.Links.PullRequest != nil {
			t.Fatalf("node a links a pull request it never had: %+v", n.Links.PullRequest)
		}
	}
}

// newMixedKit is a batch kit whose plan also carries edges: the nodes are added with the edges in the plan's first
// revision, so every node's execution is bound to the plan it was released under.
func newMixedKit(t *testing.T, nodes []batchNode, edges func(k *batchKit) []doc) *batchKit {
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
	k.putPlan("g", 0, "g-r1", append(changes, edges(k)...)...)
	for _, n := range nodes {
		r := k.reportNode("g", n.name, acceptOpts{})
		k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
			r.Event, r.Acceptance.RelationshipID, 1, "verdict-turn", k.heads[n.name], "parent", k.clock())
	}
	return k
}

// A plan that mixes pull-request and commit acceptances does not stall: p is accepted on its pull request, c by its
// commit, and q (a pull-request node) waits for c through an integrated edge. Once the batch integrated c and it was
// observed, the edge from c is satisfied, the merge check of q no longer reads c as not landed, and progress counts
// both integrated nodes with nothing blocked.
func TestAPlanMixingPullRequestAndCommitAcceptancesDoesNotStall(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	k := newMixedKit(t, []batchNode{{name: "c", files: map[string]string{"c.txt": "c\n"}}, {name: "p", files: map[string]string{"p.txt": "p\n"}}, {name: "q", files: map[string]string{"q.txt": "q\n"}}},
		func(k *batchKit) []doc {
			return []doc{addEdge("cq", "c", "q", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "dev"})}
		})
	k.wireForge(t)
	k.acceptByCommit("c")
	k.acceptByPullRequest("p", "owner/repo", 11)
	k.acceptByPullRequest("q", "owner/repo", 12)

	// before c lands, q waits for it: that is the merge check's predecessor rule, not a stall
	before, err := k.sched.Judge(ctx, "g", "q", "parent", JudgeInput{})
	if err != nil || before.Outcome != OutcomePredecessor {
		t.Fatalf("the merge check of q before c landed = %+v, %v; want %s", before, err, OutcomePredecessor)
	}
	if st := k.commitEdgeStatusOf(t, "cq"); st.Satisfied {
		t.Fatalf("the edge from c is satisfied before c landed: %+v", st)
	}

	in := k.batchIn()
	in.Nodes = []string{"c", "p"}
	batch, err := k.sched.IntegrateBatch(ctx, in, k.integrateDeps(t))
	if err != nil || len(batch.Merged) != 2 {
		t.Fatalf("dag-integrate of c and p = %+v, %v; want both merged", batch, err)
	}
	for _, node := range []string{"c", "p"} {
		if obs, err := k.sched.ObserveIntegration(ctx, "g", node, "parent", []Target{{Repository: k.repo.path, BaseRef: "dev-int"}}); err != nil || !obs.Integrated {
			t.Fatalf("observe %s on the integration branch: %+v, %v; want integrated", node, obs, err)
		}
	}

	if st := k.commitEdgeStatusOf(t, "cq"); !st.Satisfied {
		t.Fatalf("the edge from the integrated commit node c = %+v; want satisfied", st)
	}
	after, err := k.sched.Judge(ctx, "g", "q", "parent", JudgeInput{})
	if err != nil {
		t.Fatalf("the merge check of q after c landed: %v", err)
	}
	if after.Outcome == OutcomePredecessor {
		t.Fatalf("the merge check of q still reads its commit-accepted predecessor as not landed: %s", after.Reason)
	}
	if !after.Eligible() {
		t.Fatalf("the merge check of q after c landed = %+v; want eligible", after)
	}
	progress, err := k.sched.ReadProgress(ctx, "g")
	if err != nil {
		t.Fatal(err)
	}
	if progress.Cumulative.Integrated.Nodes != 2 || progress.Cumulative.Accepted.Nodes != 3 || progress.Blocked.Nodes != 0 {
		t.Fatalf("progress counts %d integrated, %d accepted and %d blocked; want 2, 3 and 0: %+v", progress.Cumulative.Integrated.Nodes, progress.Cumulative.Accepted.Nodes, progress.Blocked.Nodes, progress.Blocked)
	}
	for _, n := range progress.Nodes {
		switch n.NodeID {
		case "c":
			if n.Links.PullRequest != nil {
				t.Fatalf("the commit node c links a pull request: %+v", n.Links.PullRequest)
			}
		case "p", "q":
			if n.Links.PullRequest == nil || n.Links.PullRequest.Number == 0 {
				t.Fatalf("the pull-request node %s lost its pull request link: %+v", n.NodeID, n.Links)
			}
		}
	}
}

// A node accepted by its commit is refreshed onto the base without a pull request: in its own generation there is nothing
// to refresh (the ruling fixed the accepted head), and once the child merged dev in a generation the parent opened by hand
// and ruled verified, dag-base-refresh proves that generation's ruled head is the accepted head plus a merge of dev, reading
// no forge. The batch then merges the node at the refreshed head, and the edge out of it is satisfied once observed.
func TestCommitAcceptedNodeTakesABaseRefreshWithoutAPullRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx := context.Background()
	k := newMixedKit(t, []batchNode{{name: "a", files: map[string]string{"a.txt": "a\n"}}, {name: "z", files: map[string]string{"z.txt": "z\n"}}},
		func(k *batchKit) []doc {
			return []doc{addEdge("az", "a", "z", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "dev"})}
		})
	k.sched.Tips, k.sched.Ancestry, k.sched.PRs = newLegacyLocalTipReader(t), GitAncestry{}.Ancestry, noForge(t)
	k.acceptByCommit("a")
	acc, found, err := loadActiveAcceptance(ctx, k.s.Q(ctx), "g", "a")
	if err != nil || !found {
		t.Fatalf("the acceptance of a: %v %v", found, err)
	}

	// generation 1 is the accepted one: its ruled head is the accepted head, so there is nothing to refresh, and the refusal is not about a forge
	if _, err := k.sched.RecordBaseRefresh(ctx, "g", "a", "parent", RefreshInput{}); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "nothing to refresh") || strings.Contains(err.Error(), "forge identity") {
		t.Fatalf("a refresh in the accepted generation = %v; want nothing to refresh", err)
	}

	// dev moves, and the child merges it into its branch in generation 2, which the parent rules verified
	k.repo.commit("other.txt", "dev moved")
	k.repo.git("checkout", "-q", "feature-a")
	k.repo.git("merge", "-q", "--no-ff", "-m", "merge dev", "dev")
	refreshed := k.repo.git("rev-parse", "HEAD")
	k.repo.git("checkout", "-q", "dev")
	n, _ := nodeOf(k.snapshot("g"), "a")
	rvOpenByHand(t, k.releaseKit, acc.RelationshipID, "refresh-"+acc.RelationshipID, true)
	event2 := k.rvReportGeneration(acc.RelationshipID, "a", 2, n.CriteriaSetDigest)
	k.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
		event2, acc.RelationshipID, 2, "verdict-turn-g2", refreshed, "parent", k.clock())

	res, err := k.sched.RecordBaseRefresh(ctx, "g", "a", "parent", RefreshInput{})
	if err != nil {
		t.Fatalf("the base refresh of a commit acceptance: %v", err)
	}
	if res.HeadSHA != refreshed || res.BaseRef != "dev" || res.BaseRepository != k.repo.path || res.Generation != 2 || res.RefreshID == "" || res.Replayed {
		t.Fatalf("the refresh = %+v; want generation 2 at %s on %s dev", res, refreshed, k.repo.path)
	}
	again, err := k.sched.RecordBaseRefresh(ctx, "g", "a", "parent", RefreshInput{})
	if err != nil || !again.Replayed || again.RefreshID != res.RefreshID {
		t.Fatalf("the same refresh again = %+v, %v; want a replay", again, err)
	}
	stand, err := k.sched.standOf(ctx, k.s.Q(ctx), acc)
	if err != nil || stand.Head != refreshed || stand.Generation != 2 {
		t.Fatalf("the acceptance stands on %+v, %v; want the refreshed head of generation 2", stand, err)
	}

	// the record a batch judges is the one of the head the acceptance stands on: until the refreshed head has its own, the node is held
	in := k.batchIn()
	in.Nodes = []string{"a"}
	deps := IntegrationBatchDeps{Verify: stubVerifier(writeStubVerifier(t)), Update: updateIntegrationRef}
	if _, err := k.sched.IntegrateBatch(ctx, in, deps); refusalReasonOf(err) != "premerge_head_mismatch" {
		t.Fatalf("dag-integrate before the refreshed head has its record = %v; want premerge_head_mismatch", err)
	}
	// the record of the refreshed head is given on the commit path, naming the refreshed head over the base it merged: the same output, so a replay that appends it
	devTip := k.repo.git("rev-parse", "dev")
	attached, err := k.sched.Accept(ctx, "g", "a", "parent", premergeWithRecord(k.sched, ctx, "g", "a", "parent", AcceptInput{
		RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"},
		Commit:      &CommitRef{Head: refreshed, Base: devTip, Checkout: k.repo.path, Record: writeSealedRecord(t, k.repo.path, refreshed, devTip, k.treeOf(refreshed), "pass")},
	}))
	if err != nil || !attached.Replayed || attached.AcceptanceID != acc.AcceptanceID {
		t.Fatalf("the record of the refreshed head = %+v, %v; want a replay of acceptance %s", attached, err, acc.AcceptanceID)
	}
	batch, err := k.sched.IntegrateBatch(ctx, in, deps)
	if err != nil || len(batch.Merged) != 1 || batch.Merged[0].NodeID != "a" {
		t.Fatalf("dag-integrate of the refreshed node = %+v, %v; want a merged", batch, err)
	}
	if contained := k.repo.git("merge-base", "--is-ancestor", refreshed, "dev-int"); contained != "" {
		t.Fatalf("merge-base printed %q", contained)
	}
	if obs, err := k.sched.ObserveIntegration(ctx, "g", "a", "parent", nil); err != nil || !obs.Integrated {
		t.Fatalf("observe the refreshed node: %+v, %v; want integrated", obs, err)
	}
	if st := k.commitEdgeStatusOf(t, "az"); !st.Satisfied {
		t.Fatalf("the edge out of the refreshed commit node = %+v; want satisfied", st)
	}
}

// The base a commit acceptance is refreshed against comes from the plan or the store, never from a forge: the base ref of the
// node's own edges, else the base of the manifest it was released with, else a refusal with the reason.
func TestCommitRefreshBaseIsReadFromTheEdgesOrTheManifest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	k := newBatchKit(t, batchNode{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.acceptByCommit("a")
	acc, found, err := loadActiveAcceptance(ctx, k.s.Q(ctx), "g", "a")
	if err != nil || !found {
		t.Fatalf("the acceptance of a: %v %v", found, err)
	}
	snap := k.snapshot("g")
	if _, err := k.sched.commitRefreshBase(ctx, k.s.Q(ctx), snap, acc); refusalReasonOf(err) != "disposition_conflict" || !strings.Contains(err.Error(), "base branch") {
		t.Fatalf("a node with no edge and no manifest base = %v; want the refusal naming the base branch", err)
	}
	k.exec("UPDATE dag_input_manifests SET body_json = json_set(body_json, '$.base', json_object('repository', 'owner/repo', 'ref', 'main', 'sha', ?)) WHERE manifest_digest = ?", k.base, acc.ManifestDigest)
	if ref, err := k.sched.commitRefreshBase(ctx, k.s.Q(ctx), snap, acc); err != nil || ref != "main" {
		t.Fatalf("the manifest base = %q, %v; want main", ref, err)
	}
	k.putPlan("g", 1, "g-r2", addRelNode("z", dag.NodeNonPR), addRelNode("y", dag.NodeNonPR),
		addEdge("az", "a", "z", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "dev"}))
	if ref, err := k.sched.commitRefreshBase(ctx, k.s.Q(ctx), k.snapshot("g"), acc); err != nil || ref != "dev" {
		t.Fatalf("the edge base = %q, %v; want dev, ahead of the manifest", ref, err)
	}
	k.putPlan("g", 2, "g-r3", addEdge("ay", "a", "y", dag.EdgeIntegrated, doc{"target_repository": k.repo.path, "target_base_ref": "release"}))
	if _, err := k.sched.commitRefreshBase(ctx, k.s.Q(ctx), k.snapshot("g"), acc); refusalReasonOf(err) != "malformed_receipt" {
		t.Fatalf("two edge bases = %v; want malformed_receipt", err)
	}
}
