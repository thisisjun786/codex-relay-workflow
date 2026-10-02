package dagsched

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// The lanes that act on an accepted result must not take a stale one (contract 8.2, E-25): the merge judgement, the integrated edges of a result that landed in only some of its targets,
// and a result that is back at the criteria the plan has again after it was re-verified against others.

// invJudgeKitBelow is a judge kit whose pull request node I rests on U (the edge ui, an artifact edge): I was accepted with the inputs the product builds for it, so revising U makes U stale
// and I, which consumed U's accepted result, stale with it.
func invJudgeKitBelow(t *testing.T) *judgeKit {
	t.Helper()
	k := &judgeKit{integrationKit: newIntegrationKit(t)}
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("U", dag.NodeNonPR), addEdge("ui", "U", "I", dag.EdgeArtifactVerified, nil))
	k.invSettle("g", "U")
	repo.git("checkout", "-q", "-b", "feature")
	k.feature = repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: k.feature, PR: 5, Forge: "owner/repo", Repository: repo.path, Inputs: invRealInputs(k.releaseKit, "g", "I")})
	k.pr = PullRequest{Repository: "owner/repo", Number: 5, State: "open", HeadSHA: k.feature, BaseRef: "dev", BaseSHA: repo.git("rev-parse", "dev"), Verdict: "ready", RequiredDeclared: []string{"A", "B"}, RequiredReadable: true}
	k.setChecks("A:1:1:success", "B:2:1:success")
	return k
}

// Contract 8.2: a stale result never merges. Whether the node's own slice changed (a seed) or only what it consumed did (a descendant), the judgement refuses with disposition_conflict, names
// the stale reason and writes nothing: no history row, no merge turn. The result is judged again, and eligible, when it is current.
func TestAStaleResultIsNotJudgedForMerge(t *testing.T) {
	askForATurn := func(k *judgeKit) error {
		_, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
		if err != nil && turn != nil {
			t.Fatalf("a refusal came with a turn: %v", turn)
		}
		return err
	}
	refused := func(t *testing.T, k *judgeKit, reason string) {
		t.Helper()
		rows := k.count("SELECT COUNT(*) FROM dag_merge_checks")
		res, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
		if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), reason) || res.Eligible() {
			t.Fatalf("judge of a stale result = %v %+v, want disposition_conflict naming %s", err, res, reason)
		}
		if err := askForATurn(k); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), reason) {
			t.Fatalf("request for a stale result = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM dag_merge_checks") != rows || k.count("SELECT COUNT(*) FROM merge_turns") != 0 || k.count("SELECT COUNT(*) FROM merge_turn_ledger") != 0 {
			t.Fatal("a refusal of a stale result wrote a judgement or a turn")
		}
	}
	t.Run("a node whose own slice changed", func(t *testing.T) {
		k := newJudgeKit(t)
		if r := k.judge(); !r.Eligible() {
			t.Fatalf("before the revision = %+v", r)
		}
		k.invRevise("g", "I", "g-r2", invTitle("a changed title"))
		if n := k.read("g").node("I"); n.Disposition != invStale || n.Reason != invSliceChanged {
			t.Fatalf("I = %+v, want it stale", n)
		}
		refused(t, k, invSliceChanged)
	})
	t.Run("a node whose consumed input is stale", func(t *testing.T) {
		k := invJudgeKitBelow(t)
		if r := k.judge(); !r.Eligible() {
			t.Fatalf("before the revision = %+v", r)
		}
		k.invRevise("g", "U", "g-r3", invTitle("a changed title"))
		if n := k.read("g").node("I"); n.Disposition != invStale || n.Reason != "stale:edge:ui" {
			t.Fatalf("I = %+v, want it stale over the edge ui", n)
		}
		refused(t, k, "stale:edge:ui")
	})
	t.Run("a revision that lands between the judgement and the request", func(t *testing.T) {
		k := newJudgeKit(t)
		k.sched.testBetweenJudgeAndAsk = func() { k.invRevise("g", "I", "g-r2", invTitle("a changed title")) }
		if err := askForATurn(k); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), invSliceChanged) {
			t.Fatalf("request = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM merge_turns") != 0 || k.count("SELECT COUNT(*) FROM merge_turn_ledger") != 0 {
			t.Fatal("a turn was created for a result that went stale meanwhile")
		}
	})
}

// A result that landed in only some of its targets is not integrated, so it can be stale (E-20 exempts only what landed everywhere), and then it opens no integrated edge either, whichever
// target its landing is in: what the edge hands over is a result the plan no longer stands behind. Once it landed in every target it is exempt and the edges open.
func TestAStaleResultThatLandedInSomeTargetsOpensNoIntegratedEdge(t *testing.T) {
	k := newReleaseKit(t)
	// the shape of twoTargetPlan with the criteria the release kit registers: impl-a has an integrated edge to each of two branches of one repository
	k.putPlan("p1", 0, "p1-r1", addRelNode("impl-a", dag.NodeImplementation), addRelNode("join1", dag.NodeNonPR), addRelNode("join2", dag.NodeNonPR),
		addEdge("x1", "impl-a", "join1", dag.EdgeIntegrated, nil), addEdge("x2", "impl-a", "join2", dag.EdgeIntegrated, doc{"target_base_ref": "release"}))
	a := k.acceptNode("p1", "impl-a", pinnedOpts)
	k.integrate(a, "owner/repo", "dev", true, true)
	if n := k.read("p1").node("join1"); n.Disposition != DispReady {
		t.Fatalf("join1 on the landing = %+v, want ready", n)
	}
	k.invRevise("p1", "impl-a", "p1-r2", invTitle("a changed title"))
	after := k.read("p1")
	if n := after.node("impl-a"); n.Disposition != invStale || n.Reason != invSliceChanged {
		t.Fatalf("impl-a = %+v, want it stale: it landed in one target of two", n)
	}
	// join1's edge is satisfied by the landing and the stale gate closes it; join2's edge was never satisfied (its target did not land) and still waits for it
	if n := after.node("join1"); n.Disposition == DispReady || n.Reason != BlockedStalePredecessor || !strings.Contains(n.Detail, "x1") || !strings.Contains(n.Detail, "impl-a") {
		t.Fatalf("join1 = %+v, want %s naming the edge x1 and impl-a", n, BlockedStalePredecessor)
	}
	if n := after.node("join2"); n.Reason != WaitEdge("x2") {
		t.Fatalf("join2 = %+v, want it still waiting for the landing in release", n)
	}
	invAssertReasons(t, after)
	k.integrate(a, "owner/repo", "release", true, true)
	landed := k.read("p1")
	if got := invStaleIDs(landed); len(got) != 0 {
		t.Fatalf("a result that landed in every target is stale: %v (%s)", got, landed.brief())
	}
	for _, join := range []string{"join1", "join2"} {
		if n := landed.node(join); n.Disposition != DispReady {
			t.Fatalf("%s = %+v, want ready once impl-a landed everywhere", join, n)
		}
	}
}

// Contract E-11 and E-21: the criteria predicate is whether the acceptance's effective criteria (the newest re-verification of the same output) are the plan's. The slice digest alone cannot tell:
// the plan went back to the criteria the acceptance was made with after the output had been re-verified against others, so the slice is the consumed one again and the effective criteria are not.
// The node reads stale until the same output is re-verified against the plan's criteria, as it does after the change itself.
func TestCriteriaRolledBackAfterAReverificationIsStillStale(t *testing.T) {
	k := newReleaseKit(t)
	accepted := invSharedRoot(k)
	other := dig("other criteria")
	var event string
	if err := k.s.DB.QueryRow("SELECT event_id FROM events WHERE relationship_id = ?", accepted["A"].RelationshipID).Scan(&event); err != nil {
		t.Fatal(err)
	}
	reverify := func(seq int, digest string) {
		k.exec("UPDATE canonical_criteria SET set_digest = ? WHERE relationship_id = ?", digest, accepted["A"].RelationshipID)
		k.exec("INSERT INTO dag_acceptance_revalidations (revalidation_id, acceptance_id, criteria_set_digest, event_id, verdict_turn_id, reval_seq, revalidated_by, revalidated_at) VALUES (?, ?, ?, ?, 'vt2', ?, 'parent', 't')",
			"rv"+string(rune('0'+seq)), accepted["A"].AcceptanceID, digest, event, seq)
	}
	original := k.snapshot("sr")
	a, _ := nodeOf(original, "A")
	k.invRevise("sr", "A", "sr-r2", func(n doc) { n["criteria_set_digest"] = other })
	reverify(1, other)
	if got := invStaleIDs(k.read("sr")); len(got) != 0 {
		t.Fatalf("after the re-verification against the new criteria %v are stale", got)
	}
	// the plan goes back to the criteria the acceptance was made with
	k.invRevise("sr", "A", "sr-r3", func(n doc) {})
	if back, _ := nodeOf(k.snapshot("sr"), "A"); back.CriteriaSetDigest != a.CriteriaSetDigest || back.SliceDigest != a.SliceDigest {
		t.Fatal("the fixture did not put the plan back to the original slice")
	}
	rolledBack := k.read("sr")
	n := rolledBack.node("A")
	if n.Disposition != invStale || n.Reason != invCriteriaChanged {
		t.Fatalf("A = %+v, want stale:criteria_changed: its output stands on criteria the plan no longer has (%s)", n, rolledBack.brief())
	}
	// C is built on the A that is not verified against the plan's criteria: it is held with it, as after the change of the criteria itself
	if got := invStaleIDs(rolledBack); !reflect.DeepEqual(got, []string{"A", "C"}) || rolledBack.node("C").Reason != "stale:edge:ac" {
		t.Fatalf("stale = %v, want [A C]: %s", got, rolledBack.brief())
	}
	invAssertReasons(t, rolledBack)
	// re-verified against the plan's criteria again: current
	reverify(2, a.CriteriaSetDigest)
	if resolved := k.read("sr"); len(invStaleIDs(resolved)) != 0 {
		t.Fatalf("after the re-verification against the plan's criteria %v are stale: %s", invStaleIDs(resolved), resolved.brief())
	}
}

// The gate on an integrated edge reaches the consumer that already rests on it. U must land in two branches and landed in one (dev); I consumed that landing over the edge ui and was accepted.
// U is then revised: U is stale, so the edge ui no longer opens, and I, whose consumed value is the landing of a result the plan no longer stands behind, is stale with it: nothing is released onto
// it (its code-pinned successor T) and its pull request is not judged for the merge lane. Once U has landed everywhere it is exempt (E-20) and I is current again.
func TestAConsumerOfAStaleIntegratedResultIsStaleToo(t *testing.T) {
	k := &judgeKit{integrationKit: newIntegrationKit(t)}
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addRelNode("U", dag.NodeImplementation), addRelNode("X", dag.NodeNonPR), addRelNode("T", dag.NodeNonPR),
		addEdge("ui", "U", "I", dag.EdgeIntegrated, doc{"target_repository": repo.path}),
		addEdge("ux", "U", "X", dag.EdgeIntegrated, doc{"target_repository": repo.path, "target_base_ref": "release"}),
		addEdge("it", "I", "T", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": repo.path, "target_base_ref": "dev"}))
	u := k.acceptNode("g", "U", acceptOpts{HeadSHA: repo.git("rev-parse", "dev"), PR: 6, Forge: "owner/repo", Repository: repo.path})
	k.integrate(u, repo.path, "dev", true, true)
	repo.git("checkout", "-q", "-b", "feature")
	k.feature = repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: k.feature, PR: 5, Forge: "owner/repo", Repository: repo.path, Inputs: invRealInputs(k.releaseKit, "g", "I")})
	k.pr = PullRequest{Repository: "owner/repo", Number: 5, State: "open", HeadSHA: k.feature, BaseRef: "dev", BaseSHA: repo.git("rev-parse", "dev"), Verdict: "ready", RequiredDeclared: []string{"A", "B"}, RequiredReadable: true}
	k.setChecks("A:1:1:success", "B:2:1:success")
	if before := k.read("g"); len(invStaleIDs(before)) != 0 || before.node("I").Reason != DoneAccepted || !k.judge().Eligible() {
		t.Fatalf("before the revision: %s", before.brief())
	}
	k.invRevise("g", "U", "g-r3", invTitle("a changed title"))
	after := k.read("g")
	if got := invStaleIDs(after); !reflect.DeepEqual(got, []string{"I", "U"}) {
		t.Fatalf("stale = %v, want [I U]: %s", got, after.brief())
	}
	obj := invStaleObject(t, after, "I")
	if after.node("I").Reason != "stale:edge:ui" || invField(obj, "cause") != "predecessor_stale" || invField(obj, "predecessor_node_id") != "U" || invField(obj, "consumed_acceptance_id") != u.Acceptance.AcceptanceID {
		t.Fatalf("I = %+v %v", after.node("I"), obj)
	}
	if n := after.node("T"); n.Disposition == DispReady || n.Reason != BlockedStalePredecessor || !strings.Contains(n.Detail, "edge it") {
		t.Fatalf("T = %+v, want it held back by the stale I", n)
	}
	invAssertReasons(t, after)
	rows := k.count("SELECT COUNT(*) FROM dag_merge_checks")
	if _, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"}); refusalReason(err) != "disposition_conflict" || turn != nil || !strings.Contains(err.Error(), "stale:edge:ui") {
		t.Fatalf("request for a consumer of a stale result = %v %v", err, turn)
	}
	if k.count("SELECT COUNT(*) FROM dag_merge_checks") != rows || k.count("SELECT COUNT(*) FROM merge_turns") != 0 {
		t.Fatal("a judgement or a turn was written for a stale consumer")
	}
	// U lands in its other target: it is never stale again, and what rests on its landing is current
	k.integrate(u, repo.path, "release", true, true)
	landed := k.read("g")
	if got := invStaleIDs(landed); len(got) != 0 {
		t.Fatalf("after U landed everywhere %v are stale: %s", got, landed.brief())
	}
	if !k.judge().Eligible() {
		t.Fatal("the consumer of a landed result is judged again")
	}
}
