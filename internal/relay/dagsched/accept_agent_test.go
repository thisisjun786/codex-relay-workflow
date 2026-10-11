package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// CRW-1213 (R05): the agent path of dag-accept. The tests below drive it as the run does: the executor and reviewer calls are rows of the flow journal, the execution is the row ReleaseAgent wrote, the commits
// are real commits in a real repository and the verification record is sealed by the function the judge reads. Nothing is seeded into an acceptance table.

// wantAgentRefusal fails unless err is a refusal of reason whose text names detail.
func wantAgentRefusal(t *testing.T, what string, err error, reason contract.RefusalReason, detail string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: accepted; want %s (%s)", what, reason, detail)
		return
	}
	if got := refusalReason(err); got != string(reason) || !strings.Contains(err.Error(), detail) {
		t.Errorf("%s: %v; want the refusal %s naming %q", what, err, reason, detail)
	}
}

// agentAcceptanceRows is the number of rows of dag_agent_acceptances.
func (k *agentKit) agentAcceptanceRows() int {
	return k.count("SELECT COUNT(*) FROM dag_agent_acceptances")
}

// The reviewer of an agent acceptance is a recorded, completed, verified call whose model family differs from the family of every author pair the run observed for the node. The acceptance keeps the
// call, its thread and turn, its pair and the digest of the author pairs it was judged against; each other case is refused by name and writes nothing.
func TestAgentAcceptRequiresDistinctFamilyReview(t *testing.T) {
	t.Parallel()
	one := func(n string) agentSpec { return agentSpec{name: n, files: map[string]string{n + ".txt": n + "\n"}} }
	// the project holds six slots: the cases are two kits of five nodes
	k := newAgentKit(t, one("ok"), one("same"), one("noauthor"), one("nopair"), one("nonverdict"))
	k2 := newAgentKit(t, one("norev"), one("later"), one("second"), one("otherrun"), one("otherhead"))
	criteria := releaseCriteriaDigest()

	// a reviewer of another family than the author's is accepted, and the row keeps what the acceptance rests on
	k.author("ok", authorModel)
	reviewCall := k.review("ok", reviewerModel, "verified", criteria)
	res, err := k.accept("ok")
	if err != nil || res.AcceptanceID == "" || res.ExecutionKind != ExecutionAgent || res.ExecutionID != k.execution("ok") {
		t.Fatalf("accept ok: %+v, %v", res, err)
	}
	row, found, err := loadAgentAcceptanceByID(context.Background(), k.s.Q(context.Background()), res.AcceptanceID)
	if err != nil || !found {
		t.Fatalf("acceptance row: %v %v", found, err)
	}
	if row.ReviewerCallID != reviewCall || row.ReviewerThreadID != "thread-"+reviewCall || row.ReviewerTurnID != "turn-"+reviewCall || row.ReviewerModel != reviewerModel || row.ReviewerEffort != "high" ||
		row.AuthorPairsDigest != authorPairsDigest([]agentPair{{authorModel, "high"}}) || row.CriteriaSetDigest != criteria || row.AcceptedBy != "parent" || row.ExecutionID != k.execution("ok") ||
		row.ManifestDigest != k.releases["ok"].ManifestDigest || row.Head != k.heads["ok"] || row.Base != k.base || row.Supersedes != "" {
		t.Fatalf("the row = %+v", row)
	}

	// the reviewer's family is the author's
	k.author("same", authorModel)
	k.review("same", "claude-fake-opus", "verified", criteria)
	_, err = k.accept("same")
	wantAgentRefusal(t, "a reviewer of the author's family", err, contract.RefusalDispositionConflict, IndependenceRefused)

	// no author pair was observed: the reviewer cannot be shown to differ from it
	k.review("noauthor", reviewerModel, "verified", criteria)
	_, err = k.accept("noauthor")
	wantAgentRefusal(t, "no observed author", err, contract.RefusalDispositionConflict, IndependenceUnproven)

	// the pair a call ran on was never read back from its thread, so the reviewer's family is unknown
	k.author("nopair", authorModel)
	k.record(journalCall{role: "reviewer", node: "nopair", model: reviewerModel, head: k.heads["nopair"], verdict: "verified", criteria: criteria, noPair: true})
	_, err = k.accept("nopair")
	wantAgentRefusal(t, "a reviewer whose pair was not read back", err, contract.RefusalDispositionConflict, IndependenceUnproven)

	// the verdict decides
	k.author("nonverdict", authorModel)
	k.review("nonverdict", reviewerModel, "needs_changes", criteria)
	_, err = k.accept("nonverdict")
	wantAgentRefusal(t, "a verdict that is not verified", err, contract.RefusalDispositionConflict, ReviewerNotVerified)
	if n := k.agentAcceptanceRows(); n != 1 {
		t.Fatalf("dag_agent_acceptances holds %d rows after the refusals, want the one accepted", n)
	}

	k2.author("norev", authorModel)
	_, err = k2.accept("norev")
	wantAgentRefusal(t, "no reviewer call", err, contract.RefusalDispositionConflict, ReviewerMissing)
	// the latest verdict on the head decides
	k2.author("later", authorModel)
	k2.review("later", reviewerModel, "verified", criteria)
	k2.review("later", reviewerModel, "needs_changes", criteria)
	_, err = k2.accept("later")
	wantAgentRefusal(t, "a later verdict that withdraws the first", err, contract.RefusalDispositionConflict, ReviewerNotVerified)

	// one author of the reviewer's family among several is enough to refuse
	k2.author("second", authorModel)
	k2.record(journalCall{role: "executor-t1", node: "second", model: reviewerModel, head: k2.heads["second"]})
	k2.review("second", reviewerModel, "verified", criteria)
	_, err = k2.accept("second")
	wantAgentRefusal(t, "a second author of the reviewer's family", err, contract.RefusalDispositionConflict, IndependenceRefused)

	// the call has to be one the run that holds the execution recorded, on the head the acceptance stands on
	k2.author("otherrun", authorModel)
	k2.record(journalCall{role: "reviewer", node: "otherrun", model: reviewerModel, head: k2.heads["otherrun"], verdict: "verified", criteria: criteria, run: "run-2"})
	_, err = k2.accept("otherrun")
	wantAgentRefusal(t, "a reviewer call of another run", err, contract.RefusalDispositionConflict, ReviewerMissing)
	k2.author("otherhead", authorModel)
	k2.record(journalCall{role: "reviewer", node: "otherhead", model: reviewerModel, head: k2.base, verdict: "verified", criteria: criteria})
	_, err = k2.accept("otherhead")
	wantAgentRefusal(t, "a review of another head", err, contract.RefusalDispositionConflict, ReviewerMissing)
	if n := k2.agentAcceptanceRows(); n != 0 {
		t.Fatalf("dag_agent_acceptances holds %d rows after the refusals, want none", n)
	}
}

// The agent path requires the pre-merge record and judges it with the function the relationship path uses: a missing record, a record for another head and a record that fails are refused with the names
// they have there, and the accepted record is stored in the agent row, never in dag_acceptance_premerge.
func TestAgentAcceptRequiresPremerge(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.judged("a")
	ctx := context.Background()

	in := k.input("a")
	in.Premerge = nil
	_, err := k.sched.Accept(ctx, "g", "a", "parent", in)
	wantAgentRefusal(t, "no pre-merge record", err, contract.RefusalPremergeMissing, "pre-merge record")

	in = k.input("a")
	in.Premerge = premergeAt(k.sched, ctx, "g", "a", strings.Repeat("c", 40))
	_, err = k.sched.Accept(ctx, "g", "a", "parent", in)
	wantAgentRefusal(t, "a record for a head that is not an ancestor", err, contract.RefusalPremergeHeadMismatch, "head")
	in = k.input("a")
	in.Premerge = premergeAt(k.sched, ctx, "g", "a", k.base)
	_, err = k.sched.Accept(ctx, "g", "a", "parent", in)
	wantAgentRefusal(t, "a record for an earlier head with no afterEvaluation statement", err, "premerge_after_evaluation_missing", "afterEvaluation")

	in = k.input("a")
	in.Premerge = []byte(strings.Replace(string(in.Premerge), `"PASS"`, `"FAIL"`, 1))
	if _, err = k.sched.Accept(ctx, "g", "a", "parent", in); err == nil {
		t.Error("a record with a failing criterion was accepted")
	}
	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("%d acceptances after the refused records", n)
	}

	in = k.input("a")
	res, err := k.sched.Accept(ctx, "g", "a", "parent", in)
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	row := k.currentAcceptance("a").Agent
	if row.AcceptanceID != res.AcceptanceID || row.PremergeDigest == "" || row.PremergeJSON != string(in.Premerge) || row.PremergeEvaluatedHead != k.heads["a"] || row.VerificationDigest == "" || row.VerificationJSON == "" || row.TreeSHA != k.treeOf(k.heads["a"]) {
		t.Fatalf("the row does not hold the records it rests on: %+v", row)
	}
	if n := k.count("SELECT COUNT(*) FROM dag_acceptance_premerge") + k.count("SELECT COUNT(*) FROM dag_acceptance_verifications") + k.count("SELECT COUNT(*) FROM dag_acceptances"); n != 0 {
		t.Fatalf("the frozen acceptance tables hold %d rows of an agent acceptance", n)
	}
	// the stored record is judged again when the candidate is chosen
	cands, err := k.sched.AcceptedCandidates(ctx, "g")
	if err != nil || len(cands) != 1 || cands[0].PremergeDigest != row.PremergeDigest {
		t.Fatalf("candidates = %+v, %v", cands, err)
	}
}

// P-AV-2: the head is the commit git reads in the execution's worktree, never the commit the call names. A call naming another commit, even a real one, is refused; a call naming none is accepted at the head
// git reads; and a commit that does not descend from the base, a worktree the execution does not record and a record sealed for another tree are refused.
func TestAgentAcceptHeadFromGit(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "a", files: map[string]string{"a.txt": "a\n"}}, agentSpec{name: "b", files: map[string]string{"b.txt": "b\n"}}, agentSpec{name: "c", files: map[string]string{"c.txt": "c\n"}, unreleased: true})
	k.judged("a")
	ctx := context.Background()

	// the call names the head of another node, and the base: real commits that are not the head of this execution
	for what, named := range map[string]string{"the head of another node": k.heads["b"], "the base": k.base} {
		in := k.input("a")
		in.Commit.Head = named
		_, err := k.sched.Accept(ctx, "g", "a", "parent", in)
		wantAgentRefusal(t, "a call naming "+what, err, contract.RefusalHeadNotReceiptHead, "git reads "+k.heads["a"])
	}
	// a base the head does not descend from
	in := k.input("a")
	in.Commit.Base = k.heads["b"]
	_, err := k.sched.Accept(ctx, "g", "a", "parent", in)
	wantAgentRefusal(t, "a base the head does not descend from", err, contract.RefusalMergeBaseMismatch, "does not descend")
	// a verification record sealed for another tree
	in = k.input("a")
	in.Commit.Record = writeSealedRecord(t, k.repo.path, k.heads["b"], k.base, k.treeOf(k.heads["b"]), "pass")
	if _, err = k.sched.Accept(ctx, "g", "a", "parent", in); err == nil {
		t.Error("a record sealed for another commit was accepted")
	}
	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("%d acceptances after the refusals", n)
	}

	// a call that names no head is accepted at the head git reads in the worktree
	in = k.input("a")
	in.Commit.Head = ""
	res, err := k.sched.Accept(ctx, "g", "a", "parent", in)
	if err != nil {
		t.Fatalf("accept without a named head: %v", err)
	}
	if want := k.repo.git("-C", k.worktrees["a"], "rev-parse", "HEAD"); res.HeadSHA != want || k.currentAcceptance("a").Agent.Head != want {
		t.Fatalf("the accepted head is %s, git reads %s in the worktree", res.HeadSHA, want)
	}

	// the head follows the worktree: a commit made there afterwards is the head of the next acceptance, and the first is not touched
	k.repo.git("-C", k.worktrees["b"], "config", "user.email", "t@example.com")
	k.repo.git("-C", k.worktrees["b"], "config", "user.name", "t")
	k.author("b", authorModel)
	writeFile(t, k.worktrees["b"], "b2.txt", "more\n")
	k.repo.git("-C", k.worktrees["b"], "add", "b2.txt")
	k.repo.git("-C", k.worktrees["b"], "commit", "-q", "-m", "more")
	newHead := k.repo.git("-C", k.worktrees["b"], "rev-parse", "HEAD")
	k.heads["b"] = newHead
	k.record(journalCall{role: "reviewer", node: "b", model: reviewerModel, head: newHead, verdict: "verified", criteria: releaseCriteriaDigest()})
	in = k.input("b")
	in.Commit.Head = ""
	if res, err = k.sched.Accept(ctx, "g", "b", "parent", in); err != nil || res.HeadSHA != newHead {
		t.Fatalf("accept after a commit in the worktree: %+v, %v; want head %s", res, err, newHead)
	}

	// an execution that records no worktree has no head the relay can read
	k.worktrees["c"] = ""
	k.release("c")
	k.judged("c")
	_, err = k.accept("c")
	wantAgentRefusal(t, "an execution with no worktree", err, contract.RefusalDispositionConflict, AgentWorktreeUnrecorded)
}

// The reviewer has to have seen the criteria set the plan fixes: the digest its input carried is compared with the node's, and a review that saw another set, or none, is refused criteria_set_changed.
func TestAgentAcceptCriteriaDigestMatch(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "stale", files: map[string]string{"s.txt": "s\n"}}, agentSpec{name: "none", files: map[string]string{"n.txt": "n\n"}}, agentSpec{name: "ok", files: map[string]string{"o.txt": "o\n"}})
	for _, node := range []string{"stale", "none", "ok"} {
		k.author(node, authorModel)
	}
	k.review("stale", reviewerModel, "verified", dig("an older criteria set"))
	k.review("none", reviewerModel, "verified", "")
	k.review("ok", reviewerModel, "verified", releaseCriteriaDigest())
	_, err := k.accept("stale")
	wantAgentRefusal(t, "a review of another criteria set", err, contract.RefusalCriteriaSetChanged, "saw criteria")
	_, err = k.accept("none")
	wantAgentRefusal(t, "a review that names no criteria set", err, contract.RefusalCriteriaSetChanged, "saw criteria -")
	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("%d acceptances after the refusals", n)
	}
	res := k.mustAccept("ok")
	if got := k.currentAcceptance("ok").Agent.CriteriaSetDigest; got != releaseCriteriaDigest() || res.AcceptanceID == "" {
		t.Fatalf("the acceptance stands on criteria %s, want the plan's %s", got, releaseCriteriaDigest())
	}
}

// The second acceptance of the same execution, head and criteria set is the first row again: the call answers it (replayed) and writes nothing, also after the reviewer recorded another verdict since. A
// different output follows the current acceptance only explicitly, after which the first output is no longer the node's acceptance and cannot be accepted again.
func TestAgentAcceptReplayIdempotent(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.judged("a")
	first := k.mustAccept("a")
	before := allRows(t, k.s.DB)
	again, err := k.accept("a")
	if err != nil || !again.Replayed || again.AcceptanceID != first.AcceptanceID || again.HeadSHA != first.HeadSHA || again.EvidenceDigest != first.EvidenceDigest {
		t.Fatalf("replay: %+v, %v; want the first acceptance %+v", again, err, first)
	}
	k.review("a", reviewerModel, "needs_changes", releaseCriteriaDigest())
	if again, err = k.accept("a"); err != nil || !again.Replayed || again.AcceptanceID != first.AcceptanceID {
		t.Fatalf("replay after a later verdict: %+v, %v", again, err)
	}
	after := allRows(t, k.s.DB)
	if len(after["dag_agent_acceptances"]) != 1 || len(after["flow_calls"]) != len(before["flow_calls"])+3 {
		t.Fatalf("a replay wrote rows: %d acceptances", len(after["dag_agent_acceptances"]))
	}
	for table, rows := range before {
		if table != "flow_calls" && len(after[table]) != len(rows) {
			t.Errorf("%s holds %d rows after a replay, %d before", table, len(after[table]), len(rows))
		}
	}

	// another output of the same execution: a commit made in the worktree afterwards
	wt := k.worktrees["a"]
	k.repo.git("-C", wt, "config", "user.email", "t@example.com")
	k.repo.git("-C", wt, "config", "user.name", "t")
	writeFile(t, wt, "a2.txt", "two\n")
	k.repo.git("-C", wt, "add", "a2.txt")
	k.repo.git("-C", wt, "commit", "-q", "-m", "two")
	head2 := k.repo.git("-C", wt, "rev-parse", "HEAD")
	k.heads["a"] = head2
	k.review("a", reviewerModel, "verified", releaseCriteriaDigest())
	_, err = k.accept("a")
	wantAgentRefusal(t, "a second output without superseding", err, contract.RefusalDispositionConflict, "supersede it explicitly")
	in := k.input("a")
	in.Supersedes = "an-acceptance-that-is-not-current"
	_, err = k.sched.Accept(context.Background(), "g", "a", "parent", in)
	wantAgentRefusal(t, "superseding another acceptance", err, contract.RefusalDispositionConflict, "supersede it explicitly")
	in.Supersedes = first.AcceptanceID
	second, err := k.sched.Accept(context.Background(), "g", "a", "parent", in)
	if err != nil || second.Replayed || second.AcceptanceID == first.AcceptanceID || second.SupersededID != first.AcceptanceID || second.HeadSHA != head2 {
		t.Fatalf("supersede: %+v, %v", second, err)
	}
	if got := k.currentAcceptance("a").Agent; got.AcceptanceID != second.AcceptanceID || got.Supersedes != first.AcceptanceID {
		t.Fatalf("the current acceptance = %+v", got)
	}
	// the first output was the node's acceptance once; it is not accepted again
	k.heads["a"] = first.HeadSHA
	k.repo.git("-C", wt, "reset", "-q", "--hard", first.HeadSHA)
	_, err = k.accept("a")
	wantAgentRefusal(t, "the superseded output again", err, contract.RefusalDispositionConflict, "no longer the node's current one")
	if n := k.agentAcceptanceRows(); n != 2 {
		t.Fatalf("%d acceptances, want the 2 that were accepted", n)
	}
}

// A node that has no head (a non_pr node) is accepted by its reviewer and its actor alone, and its slot goes back in the same transaction with the reason dag_accepted, as a relationship acceptance returns
// it; an implementation node keeps its slot until it lands. A non-implementation node takes neither a commit nor a pre-merge record.
func TestAgentNonImplementationAcceptReleasesSlot(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "doc", nonPR: true, unreleased: true}, agentSpec{name: "impl", files: map[string]string{"i.txt": "i\n"}})
	k.release("doc")
	if state, _ := k.slotOf("doc"); state != "held" {
		t.Fatalf("slot = %s before the acceptance, want held", state)
	}
	k.record(journalCall{role: "architect", node: "doc", model: authorModel})
	k.record(journalCall{role: "reviewer", node: "doc", model: reviewerModel, verdict: "verified", criteria: releaseCriteriaDigest()})
	ctx := context.Background()
	rule := VerifierRule{SkillsDigest: dig("skills"), Model: "m", Effort: "none"}
	_, err := k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: rule, Premerge: []byte("{}")})
	wantAgentRefusal(t, "a pre-merge record for a non_pr node", err, contract.RefusalMalformedReceipt, "takes no pre-merge record")
	_, err = k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: rule, Commit: &CommitRef{Head: k.base, Base: k.base, Checkout: k.repo.path, Record: "x"}})
	wantAgentRefusal(t, "a commit for a non_pr node", err, contract.RefusalMalformedReceipt, "no commit to accept")
	res, err := k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: rule})
	if err != nil || !res.SlotReleased || res.HeadSHA != "" || res.AcceptanceID == "" || res.ExecutionKind != ExecutionAgent {
		t.Fatalf("accept: %+v, %v", res, err)
	}
	if state, reason := k.slotOf("doc"); state != "released" || reason != "dag_accepted" {
		t.Fatalf("slot = %s (%s) after the acceptance, want released for dag_accepted", state, reason)
	}
	if got := k.currentAcceptance("doc").Agent; got.Head != "" || got.PremergeJSON != "" || got.VerificationJSON != "" || got.ReviewerCallID == "" {
		t.Fatalf("the row of a node without a head = %+v", got)
	}
	// the replay answers the row and returns no slot again
	if again, err := k.sched.Accept(ctx, "g", "doc", "parent", AcceptInput{RuleVersion: rule}); err != nil || !again.Replayed || again.SlotReleased || again.AcceptanceID != res.AcceptanceID {
		t.Fatalf("replay: %+v, %v", again, err)
	}
	// an implementation node keeps its slot with its acceptance
	k.judged("impl")
	if r := k.mustAccept("impl"); r.SlotReleased {
		t.Fatal("an implementation node returned its slot with its acceptance")
	}
	if state, _ := k.slotOf("impl"); state != "held" {
		t.Fatalf("slot of the implementation node = %s, want held", state)
	}
}

// The plan's actor holds the acceptance: a task that is not the project's parent, and a parent that is not the orchestrator of the node's execution, are refused, and nothing is written.
func TestAgentAcceptIsTheOrchestratorsAlone(t *testing.T) {
	t.Parallel()
	k := newAgentKit(t, agentSpec{name: "a", files: map[string]string{"a.txt": "a\n"}})
	k.judged("a")
	_, err := k.sched.Accept(context.Background(), "g", "a", "stranger", k.input("a"))
	if err == nil {
		t.Fatal("a task that is not the project's parent accepted the node")
	}
	if n := k.agentAcceptanceRows(); n != 0 {
		t.Fatalf("%d acceptances", n)
	}
}
