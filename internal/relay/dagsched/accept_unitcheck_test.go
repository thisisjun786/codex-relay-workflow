package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// CRW-1456 (R1-3): the agent path of dag-accept reads two bases. Besides a reviewer call, a unit check (the completed row of the op crw.unitCheck, whose result is a reviewadapter.Result) is a basis
// when it gave a judgment (state ok) on the head under acceptance: on that commit, or before the commit on the tree of the head. Independence is judged on the model the check ran on, against the same
// author pairs a reviewer is judged against; the criteria it was given are read from its detail_json, as a reviewer's are.

const unitCheckModel = "grok-4.7-medium"

// passingUnitCheck is the result of a unit check that judged the work verified with one minor finding.
func passingUnitCheck() reviewadapter.Result {
	return reviewadapter.Result{State: reviewadapter.StateOK, Verdict: reviewadapter.VerdictVerified, EngineFamily: "grok", Model: unitCheckModel, Effort: "medium", GuideRev: "guide-1",
		PromptDigest: dig("prompt"), Findings: []reviewadapter.Finding{{ID: "0123456789abcdef", Severity: "P3", Impact: "minor_separable", File: "a.txt", Line: 1, Text: "a nit"}}}
}

// A unit check of another family than the author's that gave verified on the tree of the head, before the commit, accepts the node with no reviewer call; the acceptance names the unit check's call and
// the model and effort of its result. A check that names the head as its commit accepts the same way, and so does a check of a node without a head.
func TestAgentAcceptsOnAUnitCheckRecordWithoutAReviewerCall(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("pre"), one("post"), agentSpec{name: "doc", nonPR: true})
	criteria := releaseCriteriaDigest()
	ctx := context.Background()

	k.author("pre", authorModel)
	res := passingUnitCheck()
	res.FingerprintAfter = k.treeOf(k.heads["pre"])
	call := k.unitCheck(unitCheckCall{node: "pre", res: res, tree: k.treeOf(k.heads["pre"]), criteria: criteria})
	got, err := k.accept("pre")
	if err != nil || got.AcceptanceID == "" {
		t.Fatalf("accept on a pre-commit unit check: %+v, %v", got, err)
	}
	row, found, err := loadAgentAcceptanceByID(ctx, k.s.Q(ctx), got.AcceptanceID)
	if err != nil || !found {
		t.Fatalf("acceptance row: %v %v", found, err)
	}
	if row.ReviewerCallID != call || row.ReviewerModel != unitCheckModel || row.ReviewerEffort != "medium" || row.ReviewerThreadID != "" || row.ReviewerTurnID != "" ||
		row.CriteriaSetDigest != criteria || row.AuthorPairsDigest != authorPairsDigest([]agentPair{{authorModel, "high"}}) || row.Head != k.heads["pre"] {
		t.Fatalf("the row = %+v", row)
	}

	k.author("post", authorModel)
	res = passingUnitCheck()
	res.FingerprintAfter = k.treeOf(k.heads["post"])
	call = k.unitCheck(unitCheckCall{node: "post", res: res, commit: k.heads["post"], tree: k.treeOf(k.heads["post"]), criteria: criteria})
	if got, err = k.accept("post"); err != nil {
		t.Fatalf("accept on a unit check of the commit: %v", err)
	}
	if a := k.currentAcceptance("post").Agent; a.ReviewerCallID != call || a.ReviewerModel != unitCheckModel {
		t.Fatalf("the row = %+v", a)
	}

	// a node without a head: the check names no commit, and the acceptance stands on it
	k.record(journalCall{role: "architect", node: "doc", model: authorModel})
	call = k.unitCheck(unitCheckCall{node: "doc", res: passingUnitCheck(), tree: "fingerprint-doc", criteria: criteria})
	doc, err := k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}})
	if err != nil || !doc.SlotReleased {
		t.Fatalf("accept a node without a head on a unit check: %+v, %v", doc, err)
	}
	if a := k.currentAcceptance("doc").Agent; a.ReviewerCallID != call {
		t.Fatalf("the row of the node without a head = %+v", a)
	}
}

// A unit check is judged as a reviewer is: a model of the author's family (read from the model, or the family the result records) is refused independence_refused, a result that names no model
// independence_unproven, a verdict other than verified reviewer_not_verified, and a check given another criteria set than the plan's criteria_set_changed. Nothing is written.
func TestAgentRefusesAUnitCheckOfTheAuthorFamily(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("same"), one("family"), one("nomodel"), one("needs"), one("digest"))
	criteria := releaseCriteriaDigest()
	check := func(node string, edit func(*reviewadapter.Result), criteria string) {
		t.Helper()
		k.author(node, authorModel)
		res := passingUnitCheck()
		edit(&res)
		k.unitCheck(unitCheckCall{node: node, res: res, tree: k.treeOf(k.heads[node]), criteria: criteria})
	}

	check("same", func(r *reviewadapter.Result) { r.Model, r.EngineFamily = "claude-fake-opus", "" }, criteria)
	_, err := k.accept("same")
	wantAgentRefusal(t, "a unit check of the author's family", err, contract.RefusalDispositionConflict, IndependenceRefused)

	check("family", func(r *reviewadapter.Result) { r.Model, r.EngineFamily = reviewerModel, "claude" }, criteria)
	_, err = k.accept("family")
	wantAgentRefusal(t, "a unit check whose recorded family is the author's", err, contract.RefusalDispositionConflict, IndependenceRefused)

	check("nomodel", func(r *reviewadapter.Result) { r.Model, r.EngineFamily = "", "" }, criteria)
	_, err = k.accept("nomodel")
	wantAgentRefusal(t, "a unit check that names no model", err, contract.RefusalDispositionConflict, IndependenceUnproven)

	check("needs", func(r *reviewadapter.Result) { r.Verdict = reviewadapter.VerdictNeedsChanges }, criteria)
	_, err = k.accept("needs")
	wantAgentRefusal(t, "a unit check that asked for changes", err, contract.RefusalDispositionConflict, ReviewerNotVerified)

	check("digest", func(*reviewadapter.Result) {}, dig("another criteria set"))
	_, err = k.accept("digest")
	wantAgentRefusal(t, "a unit check given another criteria set", err, contract.RefusalCriteriaSetChanged, "saw criteria")

	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("dag_agent_acceptances holds %d rows after the refusals, want none", n)
	}
}

// With no reviewer call and no unit check that judged the head, the node is refused reviewer_missing: only the executor call, a check of another tree, a check of another commit, a check that gave
// no judgment (quota) and a check another run recorded are no basis.
func TestAgentRefusesWhenNeitherBasisIsRecorded(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("none"), one("othertree"), one("othercommit"), one("quota"), one("otherrun"))
	criteria := releaseCriteriaDigest()

	k.author("none", authorModel)
	_, err := k.accept("none")
	wantAgentRefusal(t, "only the executor call", err, contract.RefusalDispositionConflict, ReviewerMissing)

	k.author("othertree", authorModel)
	k.unitCheck(unitCheckCall{node: "othertree", res: passingUnitCheck(), tree: k.treeOf(k.base), criteria: criteria})
	_, err = k.accept("othertree")
	wantAgentRefusal(t, "a pre-commit unit check of another tree", err, contract.RefusalDispositionConflict, ReviewerMissing)

	k.author("othercommit", authorModel)
	k.unitCheck(unitCheckCall{node: "othercommit", res: passingUnitCheck(), commit: k.base, tree: k.treeOf(k.heads["othercommit"]), criteria: criteria})
	_, err = k.accept("othercommit")
	wantAgentRefusal(t, "a unit check of another commit", err, contract.RefusalDispositionConflict, ReviewerMissing)

	k.author("quota", authorModel)
	quota := passingUnitCheck()
	quota.State, quota.Verdict, quota.Findings, quota.Detail = reviewadapter.StateQuota, "", nil, "usage limit"
	k.unitCheck(unitCheckCall{node: "quota", res: quota, tree: k.treeOf(k.heads["quota"]), criteria: criteria})
	_, err = k.accept("quota")
	wantAgentRefusal(t, "a lone unit check that hit the quota", err, contract.RefusalDispositionConflict, ReviewerMissing)

	k.author("otherrun", authorModel)
	k.unitCheck(unitCheckCall{node: "otherrun", res: passingUnitCheck(), tree: k.treeOf(k.heads["otherrun"]), criteria: criteria, run: "run-2"})
	_, err = k.accept("otherrun")
	wantAgentRefusal(t, "a unit check of another run", err, contract.RefusalDispositionConflict, ReviewerMissing)

	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("dag_agent_acceptances holds %d rows after the refusals, want none", n)
	}
}

// The reviewer basis stays: a verified reviewer call of the execution's run accepts as before; a newer unit check that gave no judgment (no_answer) withdraws nothing, so the earlier reviewer verdict
// still decides; a newer unit check that asked for changes withdraws it; a reviewer call of another run is no basis.
func TestAgentStillAcceptsOnAReviewerVerdictOfAnEarlierRun(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("plain"), one("older"), one("withdrawn"), one("otherrun"))
	criteria := releaseCriteriaDigest()

	k.author("plain", authorModel)
	rev := k.review("plain", reviewerModel, "verified", criteria)
	if _, err := k.accept("plain"); err != nil {
		t.Fatalf("accept on a reviewer verdict: %v", err)
	}
	if a := k.currentAcceptance("plain").Agent; a.ReviewerCallID != rev || a.ReviewerModel != reviewerModel || a.ReviewerThreadID != "thread-"+rev {
		t.Fatalf("the row = %+v", a)
	}

	k.author("older", authorModel)
	rev = k.review("older", reviewerModel, "verified", criteria)
	silent := passingUnitCheck()
	silent.State, silent.Verdict, silent.Findings = reviewadapter.StateNoAnswer, "", nil
	k.unitCheck(unitCheckCall{node: "older", res: silent, tree: k.treeOf(k.heads["older"]), criteria: criteria})
	if _, err := k.accept("older"); err != nil {
		t.Fatalf("accept on an earlier reviewer verdict under a later silent unit check: %v", err)
	}
	if a := k.currentAcceptance("older").Agent; a.ReviewerCallID != rev || a.ReviewerModel != reviewerModel {
		t.Fatalf("the row = %+v", a)
	}

	k.author("withdrawn", authorModel)
	k.review("withdrawn", reviewerModel, "verified", criteria)
	needs := passingUnitCheck()
	needs.Verdict = reviewadapter.VerdictNeedsChanges
	k.unitCheck(unitCheckCall{node: "withdrawn", res: needs, tree: k.treeOf(k.heads["withdrawn"]), criteria: criteria})
	_, err := k.accept("withdrawn")
	wantAgentRefusal(t, "a later unit check that asked for changes", err, contract.RefusalDispositionConflict, ReviewerNotVerified)

	k.author("otherrun", authorModel)
	k.record(journalCall{role: "reviewer", node: "otherrun", model: reviewerModel, head: k.heads["otherrun"], verdict: "verified", criteria: criteria, run: "run-2"})
	_, err = k.accept("otherrun")
	wantAgentRefusal(t, "a reviewer call of another run", err, contract.RefusalDispositionConflict, ReviewerMissing)
}

// The family a unit check declares is read in canonical form (trimmed, lower case), as an author's family is read from its model: ' Claude ' against an
// author of family claude is the author's own family (independence_refused). A declared family that is blank after the trim, or is not a family name (it
// carries a provider prefix, a dash or a space), is unknown (independence_unproven), as the family of a check that names no model is. Nothing is written.
func TestAgentReadsTheDeclaredFamilyOfAUnitCheckCanonically(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("spelled"), one("upper"), one("blank"), one("prefixed"), one("model"), one("spaced"))
	criteria := releaseCriteriaDigest()
	check := func(node, family string) {
		t.Helper()
		k.author(node, authorModel)
		res := passingUnitCheck()
		res.Model, res.EngineFamily = reviewerModel, family
		k.unitCheck(unitCheckCall{node: node, res: res, tree: k.treeOf(k.heads[node]), criteria: criteria})
	}

	check("spelled", " Claude ")
	_, err := k.accept("spelled")
	wantAgentRefusal(t, "a unit check declaring ' Claude ' over an author of family claude", err, contract.RefusalDispositionConflict, IndependenceRefused)

	check("upper", "CLAUDE")
	_, err = k.accept("upper")
	wantAgentRefusal(t, "a unit check declaring 'CLAUDE'", err, contract.RefusalDispositionConflict, IndependenceRefused)

	for node, family := range map[string]string{"blank": "  \t", "prefixed": "anthropic/claude", "model": "claude-opus", "spaced": "cla ude"} {
		check(node, family)
		_, err = k.accept(node)
		wantAgentRefusal(t, "a unit check declaring the family "+family, err, contract.RefusalDispositionConflict, IndependenceUnproven)
	}

	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("dag_agent_acceptances holds %d rows after the refusals, want none", n)
	}
}

// CRW-1459: a split node's sub-tasks are committed with no unit check between them, and the node's acceptance rests on the one check of
// its whole change, the committed check that names the head: with no such check the node is refused reviewer_missing (its checkpoint
// commits were never checked), and the verified check of the whole change on the head accepts it.
func TestAgentTakesTheCheckOfTheWholeChangeAsTheBasis(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	k := newAgentKit(t, one("unchecked"), one("whole"))
	criteria := releaseCriteriaDigest()

	k.author("unchecked", authorModel)
	_, err := k.accept("unchecked")
	wantAgentRefusal(t, "a split node with no check of its whole change", err, contract.RefusalDispositionConflict, ReviewerMissing)

	k.author("whole", authorModel)
	call := k.unitCheck(unitCheckCall{node: "whole", res: passingUnitCheck(), commit: k.heads["whole"], tree: k.treeOf(k.heads["whole"]), criteria: criteria})
	if _, err := k.accept("whole"); err != nil {
		t.Fatalf("accept on the check of the whole change: %v", err)
	}
	if a := k.currentAcceptance("whole").Agent; a.ReviewerCallID != call {
		t.Fatalf("the acceptance stands on %s, want the check of the whole change %s", a.ReviewerCallID, call)
	}
}
