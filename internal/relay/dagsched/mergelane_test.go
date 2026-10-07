package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// laneKit has four implementation nodes accepted on real branches of one repository, which its forge repository stands for: I and D are cut from the current base and eligible, S was cut before the base
// moved (its head lacks the new tip) and V is a node whose required check keeps failing. Their pull requests are scripted, and so are the heads the lane's check compares with (pulls, seeded from the
// branch heads, never from a merge turn); the base branch, the ancestry, the merge turns and the landing are the real code.
type laneKit struct {
	*integrationKit
	heads map[string]string
	prs   map[string]PullRequest
	pulls *forgeLanePulls
}

func newLaneKit(t *testing.T) *laneKit {
	t.Helper()
	k := &laneKit{integrationKit: newIntegrationKit(t), heads: map[string]string{}, prs: map[string]PullRequest{}}
	k.pulls = k.lanePulls()
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-lane", addRelNode("S", dag.NodeImplementation), addRelNode("V", dag.NodeImplementation))
	cut := func(node string) {
		repo.git("checkout", "-q", "-b", "branch-"+node, "dev")
		k.heads[node] = repo.commit(strings.ToLower(node)+".txt", node)
		repo.git("checkout", "-q", "dev")
	}
	cut("S") // before the base moves
	repo.commit("moved.txt", "the base moves on")
	for _, node := range []string{"I", "D", "V"} {
		cut(node)
	}
	for i, node := range []string{"I", "D", "S", "V"} {
		number := int64(5 + i)
		k.declare("g", node, strings.ToLower(node)+".txt")
		k.acceptOnForge("g", node, acceptOpts{HeadSHA: k.heads[node], PR: number})
		k.pulls.show(number, k.heads[node])
		k.prs[node] = PullRequest{Repository: "owner/repo", Number: number, State: "open", HeadSHA: k.heads[node], BaseRef: "dev", BaseSHA: repo.git("rev-parse", "dev"), Verdict: "ready",
			RequiredDeclared: []string{"A", "B"}, RequiredReadable: true}
		k.checks(node, "A:1:1:success", "B:2:1:success")
	}
	return k
}

func (k *laneKit) checks(node string, specs ...string) {
	k.t.Helper()
	pr := k.prs[node]
	pr.Checks = nil
	for _, spec := range specs {
		f := strings.Split(spec, ":")
		pr.Checks = append(pr.Checks, Check{Name: f[0], RunID: f[1], Attempt: int64(f[2][0] - '0'), Conclusion: f[3], HeadSHA: pr.HeadSHA})
	}
	k.prs[node] = pr
	k.forge.by["owner/repo#"+string(rune('0'+pr.Number))] = pr
}

func (k *laneKit) request(node string) (JudgeResult, map[string]any, error) {
	return k.sched.RequestMergeTurn(context.Background(), "g", node, "parent", MergeRequestInput{Host: "host"})
}

// Criterion c8 with D-16: a pull request that is not eligible never gets a merge turn (a stale base, a failing required check, a retry still open), the two that are eligible queue in the
// order they asked, and the lane's own check and landing run unchanged on the eligible head. Afterwards every landed tree has a judgement of eligibility for the very head that landed.
func TestMergeLaneStaysFIFOAndLandsOnlyJudgedTrees(t *testing.T) {
	t.Parallel()
	k := newLaneKit(t)
	repo := k.repo
	// S: the head was cut before the base moved
	if _, turn, err := k.request("S"); refusalReason(err) != "merge_currency_stale" || turn != nil {
		t.Fatalf("a head without the base tip = %v %v", err, turn)
	}
	// V: its required check fails, is retried once on the same head and fails again
	k.checks("V", "A:1:1:failure", "B:2:1:success")
	if res, turn, err := k.request("V"); refusalReason(err) != "disposition_conflict" || turn != nil || res.Outcome != OutcomeRetrySameSHA {
		t.Fatalf("the first failure = %v %+v %v", err, res, turn)
	}
	k.checks("V", "A:1:2:failure", "B:2:1:success")
	if res, turn, err := k.request("V"); refusalReason(err) != "disposition_conflict" || turn != nil || res.Outcome != OutcomeEvicted {
		t.Fatalf("the second failure = %v %+v %v", err, res, turn)
	}
	k.checks("V", "A:1:3:success", "B:2:1:success")
	if res, _, err := k.request("V"); refusalReason(err) != "disposition_conflict" || res.Outcome != OutcomeEvicted {
		t.Fatalf("an evicted head stays evicted = %v %+v", err, res)
	}
	if n := k.count("SELECT COUNT(*) FROM merge_turns"); n != 0 {
		t.Fatalf("%d merge turns for pull requests that are not eligible", n)
	}
	// another project holds the target and a third asks after us: our request waits behind the first, and the lane (not the DAG) orders the queue by when each asked
	k.parentOf("P-B", "parent-b")
	k.parentOf("P-C", "parent-c")
	ctx := context.Background()
	service := forgeLaneService(k.sched, k.s, k.pulls)
	other, err := service.Request(ctx, forgeKitRepository, "dev", "P-B", "parent-b", "host-b", strings.Repeat("b", 40), true)
	if err != nil || other["state"] != "holding" {
		t.Fatalf("the other project's turn = %v %v", err, other)
	}
	firstJudged, first, err := k.request("I")
	if err != nil || !firstJudged.Eligible() || first["state"] != "waiting" {
		t.Fatalf("I = %v %+v %v", err, firstJudged, first)
	}
	third, err := service.Request(ctx, forgeKitRepository, "dev", "P-C", "parent-c", "host-c", strings.Repeat("c", 40), true)
	if err != nil || third["state"] != "waiting" {
		t.Fatalf("the third project's turn = %v %v", err, third)
	}
	// asking again is the same turn; D cannot take a solo grant while I's is open. Since CRW-898 (the
	// management decision of 10-07 13:5x) D's request is kept as a member-only waiting turn of the same
	// parent instead of being refused, and it still never receives I's turn.
	if _, again, err := k.request("I"); err != nil || again["turnId"] != first["turnId"] {
		t.Fatalf("I again = %v %v", err, again)
	}
	if res, turn, err := k.request("D"); err != nil || !res.Eligible() || turn == nil || turn["turnId"] == first["turnId"] || turn["state"] != "member_waiting" {
		t.Fatalf("D while I's turn is open = %v %+v %v", err, res, turn)
	}
	released, err := service.Release(ctx, other["turnId"].(string), "parent-b", "returned", "done", "")
	if err != nil {
		t.Fatal(err)
	}
	if next, _ := released["promoted"].(map[string]any); next == nil || next["turnId"] != first["turnId"] || next["state"] != "holding" {
		t.Fatalf("the turn after the first project's went to %v, want ours %v (it asked before the third project)", next, first["turnId"])
	}
	grant, _ := released["promoted"].(map[string]any)["grant"].(map[string]any)
	if _, err := service.Acknowledge(ctx, first["turnId"].(string), "parent", grant["grantId"].(string), "re-read the store before merging"); err != nil {
		t.Fatalf("acknowledging the grant: %v", err)
	}
	// the lane's own check: it reads the base itself, compares the head with the pull request the forge shows, and begins the merge
	forgeLaneCheck(t, service, k.pulls, first["turnId"].(string), k.heads["I"], repo.git("rev-parse", "dev"), forgeLaneChecks(k.prs["I"].Checks), forgeLaneGreen(), []string{"A", "B"})
	// the merge happens on the forge; here it is a merge commit on the base branch
	repo.git("merge", "-q", "--no-ff", "-m", "merge I", "branch-I")
	landedAt := repo.git("rev-parse", "dev")
	landed, err := service.Land(ctx, first["turnId"].(string), "parent", landedAt, "", "merged by the forge", k.pulls)
	if err != nil {
		t.Fatalf("landing I: %v", err)
	}
	if promoted, _ := landed["promoted"].(map[string]any); promoted == nil || promoted["turnId"] != third["turnId"] || promoted["state"] != "holding" {
		t.Fatalf("the turn after I's landing went to %v, want the third project's %v", promoted, third["turnId"])
	}
	// D may ask now that I's turn is closed, but its head was cut before I landed: its judgement says the base moved
	if res, _, err := k.request("D"); refusalReason(err) != "merge_currency_stale" || res.Outcome != OutcomeStaleBase {
		t.Fatalf("D after the base moved = %v %+v", err, res)
	}
	// nothing landed that was not judged eligible at that head
	if n := k.count("SELECT COUNT(*) FROM merge_turns WHERE state = 'landed' AND project_key = 'P-TEST'"); n != 1 {
		t.Fatalf("%d landed turns of the project", n)
	}
	if n := k.count("SELECT COUNT(*) FROM merge_turns m WHERE m.state = 'landed' AND m.project_key = 'P-TEST' AND NOT EXISTS (SELECT 1 FROM dag_merge_checks c JOIN dag_acceptances a ON a.acceptance_id = c.acceptance_id" +
		" WHERE c.outcome = 'eligible' AND c.observed_head_sha = m.candidate_head AND a.head_sha = m.candidate_head)"); n != 0 {
		t.Fatalf("%d landed trees without a judgement of eligibility", n)
	}
	if n := k.count("SELECT COUNT(*) FROM merge_turns WHERE candidate_head IN (?, ?)", k.heads["S"], k.heads["V"]); n != 0 {
		t.Fatalf("%d merge turns for the stale-base and evicted heads", n)
	}
	// every turn of the lane is on the forge repository the acceptances name: none is on a checkout path
	if n := k.count("SELECT COUNT(*) FROM merge_turns WHERE repository <> ?", forgeKitRepository); n != 0 {
		t.Fatalf("%d merge turns on a repository other than the forge", n)
	}
}

func (k *laneKit) parentOf(project, task string) {
	k.t.Helper()
	now := k.clock()
	if err := storeseed.InsertScopeBinding(context.Background(), k.s, store.ScopeBindingsRow{BindingID: "bind-" + task, Role: "parent", ScopeKind: "project", ScopeKey: project,
		TaskID: task, HostID: "host", Status: "active", Revision: 1, CreatedAt: now, UpdatedAt: now}); err != nil {
		k.t.Fatal(err)
	}
}
