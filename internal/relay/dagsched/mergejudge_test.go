package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// judgeKit is an integration kit with node I accepted at the head of a real branch ("feature", cut from dev) whose pull request is scripted: the forge answers what the test says, while the
// base branch and the ancestry come from the real repository.
type judgeKit struct {
	*integrationKit
	feature string
	pr      PullRequest
}

func newJudgeKit(t *testing.T) *judgeKit {
	t.Helper()
	k := &judgeKit{integrationKit: newIntegrationKit(t)}
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	k.feature = repo.commit("feature.txt", "feature")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: k.feature, PR: 5, Forge: "owner/repo", Repository: repo.path})
	k.pr = PullRequest{Repository: "owner/repo", Number: 5, State: "open", HeadSHA: k.feature, BaseRef: "dev", BaseSHA: repo.git("rev-parse", "dev"), Verdict: "ready", RequiredDeclared: []string{"A", "B"}, RequiredReadable: true}
	k.setChecks("A:1:1:success", "B:2:1:success")
	return k
}

// setChecks scripts the checks of the pull request's head: each entry is name:runId:attempt:conclusion[:stamp] (an empty conclusion has not finished).
func (k *judgeKit) setChecks(specs ...string) {
	k.t.Helper()
	k.pr.Checks = nil
	for _, spec := range specs {
		f := strings.SplitN(spec, ":", 5)
		attempt := int64(f[2][0] - '0')
		check := Check{Name: f[0], RunID: f[1], Attempt: attempt, Conclusion: f[3], HeadSHA: k.pr.HeadSHA}
		if len(f) == 5 {
			check.Stamp = f[4]
		}
		k.pr.Checks = append(k.pr.Checks, check)
	}
	k.forge.by["owner/repo#5"] = k.pr
}

func (k *judgeKit) judge() JudgeResult {
	k.t.Helper()
	res, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
	if err != nil {
		k.t.Fatalf("judge: %v", err)
	}
	return res
}

func (k *judgeKit) history() []string {
	k.t.Helper()
	rows, err := k.s.DB.Query("SELECT outcome || ':' || round_no FROM dag_merge_checks ORDER BY acceptance_id, check_seq")
	if err != nil {
		k.t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			k.t.Fatal(err)
		}
		out = append(out, s)
	}
	return out
}

// Criterion c8 and decision D-12: a required check that fails is retried once on the same head and a second, different failure evicts the node for good. A failure is counted only when every
// required check has finished, a non-required check never counts, the same snapshot read again is not a second failure, and a head that is evicted stays evicted.
func TestMergeEligibilityRetryOnceThenEvict(t *testing.T) {
	t.Run("a flaky check passes after one retry", func(t *testing.T) {
		k := newJudgeKit(t)
		// A is red while B is still running: nothing is counted yet
		k.setChecks("A:1:1:failure", "B:2:1:")
		if r := k.judge(); r.Outcome != OutcomeChecksPending || r.Round != 1 || len(r.FailedRequired) != 0 {
			t.Fatalf("a red job beside a running one = %+v", r)
		}
		k.setChecks("A:1:1:failure", "B:2:1:success")
		first := k.judge()
		if first.Outcome != OutcomeRetrySameSHA || first.Round != 1 || strings.Join(first.FailedRequired, ",") != "A|1|1" || first.Replayed {
			t.Fatalf("the first failure = %+v", first)
		}
		// the same snapshot again is the same judgement
		if again := k.judge(); again.Outcome != OutcomeRetrySameSHA || !again.Replayed || again.CheckSeq != first.CheckSeq {
			t.Fatalf("the same snapshot = %+v", again)
		}
		// the retry (the same run, the next attempt) passes, and a non-required check is red
		k.setChecks("A:1:2:success", "B:2:1:success", "lint:3:1:failure")
		done := k.judge()
		if !done.Eligible() || done.Round != 2 || !strings.Contains(done.Reason, "flaky") {
			t.Fatalf("after the retry = %+v", done)
		}
		if got := strings.Join(k.history(), " "); got != "checks_pending:1 retry_same_sha:1 eligible:2" {
			t.Fatalf("history = %s", got)
		}
	})
	t.Run("the newest attempt of a run decides", func(t *testing.T) {
		k := newJudgeKit(t)
		// a listing that still carries the first, failed attempt of a run beside its second, passing one
		k.setChecks("A:1:1:failure", "A:1:2:success", "B:2:1:success")
		if r := k.judge(); !r.Eligible() || len(r.FailedRequired) != 0 {
			t.Fatalf("the retried run = %+v", r)
		}
	})
	t.Run("a failure seen after the checks were seen running again is a second failure", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure", "B:2:1:success")
		k.judge()
		// the retry has started: the run is in progress, then fails with an identity the forge gives no way to tell from the first
		k.setChecks("A:1:1:", "B:2:1:success")
		if r := k.judge(); r.Outcome != OutcomeChecksPending {
			t.Fatalf("running = %+v", r)
		}
		k.setChecks("A:1:1:failure", "B:2:1:success")
		if r := k.judge(); r.Outcome != OutcomeEvicted {
			t.Fatalf("failed again after running = %+v", r)
		}
	})
	t.Run("another check running again is not another failure of this one", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure", "B:2:1:success")
		if r := k.judge(); r.Outcome != OutcomeRetrySameSHA {
			t.Fatalf("first = %+v", r)
		}
		// B runs again while A still shows the one failure it had, then B passes
		k.setChecks("A:1:1:failure", "B:2:1:")
		if r := k.judge(); r.Outcome != OutcomeChecksPending {
			t.Fatalf("B running = %+v", r)
		}
		k.setChecks("A:1:1:failure", "B:2:2:success")
		if r := k.judge(); r.Outcome != OutcomeRetrySameSHA || r.Round != 1 {
			t.Fatalf("A has failed once = %+v", r)
		}
	})
	t.Run("a second failure evicts for good", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure", "B:2:1:success")
		if r := k.judge(); r.Outcome != OutcomeRetrySameSHA {
			t.Fatalf("first = %+v", r)
		}
		k.setChecks("A:1:2:failure", "B:2:1:success")
		evicted := k.judge()
		if evicted.Outcome != OutcomeEvicted || evicted.Round != 2 || strings.Join(evicted.FailedRequired, ",") != "A|1|2" {
			t.Fatalf("second failure = %+v", evicted)
		}
		// nothing later brings the head back: green checks, the same snapshot, and the reading
		k.setChecks("A:1:3:success", "B:2:1:success")
		if later := k.judge(); later.Outcome != OutcomeEvicted || !later.Replayed || later.CheckSeq != evicted.CheckSeq {
			t.Fatalf("after green checks = %+v", later)
		}
		if n := k.read("g").node("I"); n.Reason != BlockedEvicted {
			t.Fatalf("I = %+v, want blocked:evicted", n)
		}
		if got := strings.Join(k.history(), " "); got != "retry_same_sha:1 evicted:2" {
			t.Fatalf("history = %s", got)
		}
	})
	t.Run("another check failing in the retry round also evicts, an unrelated one does not", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure", "B:2:1:success")
		k.judge()
		// a non-required check finishing red changes the snapshot but not the required failures
		k.setChecks("A:1:1:failure", "B:2:1:success", "lint:3:1:failure")
		if r := k.judge(); r.Outcome != OutcomeRetrySameSHA {
			t.Fatalf("an unrelated red check = %+v", r)
		}
		k.setChecks("A:1:2:success", "B:2:2:failure")
		if r := k.judge(); r.Outcome != OutcomeEvicted {
			t.Fatalf("B failing in the retry round = %+v", r)
		}
	})
}

// The history appends only what changed: the same judgement again writes nothing, a change of criteria is a row of its own and so is the return.
func TestMergeJudgeHistoryAppends(t *testing.T) {
	k := newJudgeKit(t)
	first := k.judge()
	if !first.Eligible() || first.CheckSeq != 1 {
		t.Fatalf("first = %+v", first)
	}
	if again := k.judge(); !again.Replayed || again.CheckSeq != 1 {
		t.Fatalf("again = %+v", again)
	}
	digest := dig("registered again")
	k.exec("UPDATE canonical_criteria SET set_digest = ?", digest)
	if r := k.judge(); r.Outcome != OutcomeStaleCriteria || r.CheckSeq != 2 {
		t.Fatalf("criteria registered again = %+v", r)
	}
	node, _ := nodeOf(k.snapshot("g"), "I")
	k.exec("UPDATE canonical_criteria SET set_digest = ?", node.CriteriaSetDigest)
	if r := k.judge(); !r.Eligible() || r.CheckSeq != 3 {
		t.Fatalf("criteria restored = %+v", r)
	}
	if got := strings.Join(k.history(), " "); got != "eligible:1 stale_criteria:1 eligible:1" {
		t.Fatalf("history = %s", got)
	}
}

// Rule 4 asks whether the base tip is IN the head, which the repository can answer; the pull request's own base field is only what the forge shows and says nothing about what the checks
// ran against, so it is neither trusted nor needed.
func TestMergeStaleBaseIsHeadContainsTip(t *testing.T) {
	k := newJudgeKit(t)
	repo := k.repo
	// the base moves on after the branch was cut; the forge still shows the new tip as the pull request's base, as it does for any open pull request
	tip := repo.commit("newer.txt", "newer")
	k.pr.BaseSHA = tip
	k.setChecks("A:1:1:success", "B:2:1:success")
	stale := k.judge()
	if stale.Outcome != OutcomeStaleBase || stale.BaseTipSHA != tip || !strings.Contains(stale.Reason, tip) {
		t.Fatalf("a head that lacks the new tip = %+v", stale)
	}
	// a head built on the new tip is eligible although the field the forge shows is an old commit
	repo.git("checkout", "-q", "-b", "second", tip)
	second := repo.commit("second.txt", "second")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "D", "second.txt")
	k.acceptNode("g", "D", acceptOpts{HeadSHA: second, PR: 6, Forge: "owner/repo", Repository: repo.path})
	pr := k.pr
	pr.Number, pr.HeadSHA, pr.BaseSHA = 6, second, strings.Repeat("0", 40)
	pr.Checks = []Check{{Name: "A", RunID: "1", Attempt: 1, Conclusion: "success", HeadSHA: second}, {Name: "B", RunID: "2", Attempt: 1, Conclusion: "success", HeadSHA: second}}
	k.forge.by["owner/repo#6"] = pr
	if r, err := k.sched.Judge(context.Background(), "g", "D", "parent", JudgeInput{}); err != nil || !r.Eligible() {
		t.Fatalf("a head that contains the tip = %v %+v", err, r)
	}
	// the head commit is not in the local repository (the pull request was never fetched): the question cannot be answered, which is the host's failure and writes nothing
	before := k.count("SELECT COUNT(*) FROM dag_merge_checks")
	absent := strings.Repeat("9", 40)
	k.exec("UPDATE dag_acceptances SET head_sha = ? WHERE node_id = 'I'", absent)
	k.pr.HeadSHA = absent
	k.setChecks("A:1:1:success", "B:2:1:success")
	if _, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{}); err == nil || refusalReason(err) != "not a refusal: "+err.Error() {
		t.Fatalf("an absent head = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_merge_checks") != before {
		t.Fatal("a judgement that could not be made wrote a row")
	}
}

// Each rule is decided in its order and the cases that cannot be judged write nothing.
func TestMergeEligibilityStaleAndOrder(t *testing.T) {
	t.Run("the pull request moved after the acceptance", func(t *testing.T) {
		k := newJudgeKit(t)
		k.pr.HeadSHA = strings.Repeat("2", 40)
		k.setChecks("A:1:1:success", "B:2:1:success")
		r := k.judge()
		if r.Outcome != OutcomeStaleHead || r.ObservedHeadSHA != k.pr.HeadSHA || r.HeadSHA != k.feature {
			t.Fatalf("judge = %+v", r)
		}
		var observed string
		if err := k.s.DB.QueryRow("SELECT observed_head_sha FROM dag_merge_checks").Scan(&observed); err != nil || observed != k.pr.HeadSHA {
			t.Fatalf("observed = %q %v", observed, err)
		}
	})
	for _, kind := range []string{"integrated", "pinned"} {
		t.Run("a predecessor that has not landed ("+kind+")", func(t *testing.T) {
			k := newJudgeKit(t)
			repo := k.repo
			extra := doc{"target_repository": repo.path, "target_base_ref": "dev"}
			edgeKind := "integrated"
			if kind == "pinned" {
				edgeKind = "artifact_verified"
				extra["pins_code_head"] = true
			}
			k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addEdge("id", "I", "D", edgeKind, extra))
			tip := repo.git("rev-parse", "dev")
			repo.git("checkout", "-q", "-b", "stacked", k.feature)
			second := repo.commit("stacked.txt", "stacked")
			repo.git("checkout", "-q", "dev")
			_ = tip
			k.declare("g", "D", "stacked.txt")
			a := k.acceptNode("g", "D", acceptOpts{HeadSHA: second, PR: 6, Forge: "owner/repo", Repository: repo.path})
			pr := k.pr
			pr.Number, pr.HeadSHA = 6, second
			pr.Checks = []Check{{Name: "A", RunID: "1", Attempt: 1, Conclusion: "success", HeadSHA: second}, {Name: "B", RunID: "2", Attempt: 1, Conclusion: "success", HeadSHA: second}}
			k.forge.by["owner/repo#6"] = pr
			judgeD := func() JudgeResult {
				r, err := k.sched.Judge(context.Background(), "g", "D", "parent", JudgeInput{})
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			if r := judgeD(); r.Outcome != OutcomePredecessor || !strings.Contains(r.Reason, "I") {
				t.Fatalf("predecessor not landed = %+v", r)
			}
			_ = a
			// the predecessor lands: its head is observed in the target and the parent marked it merged
			k.integrate(accepted{Acceptance: mustActive(t, k.fixture, "g", "I"), Event: "evt-rel-g-I"}, repo.path, "dev", true, true)
			if r := judgeD(); !r.Eligible() {
				t.Fatalf("predecessor landed = %+v", r)
			}
		})
	}
	t.Run("what cannot be judged writes nothing", func(t *testing.T) {
		k := newJudgeKit(t)
		for name, c := range map[string]struct {
			mutate func(*PullRequest)
			reason string
		}{
			"required checks unreadable": {func(p *PullRequest) { p.RequiredReadable = false }, "merge_evidence_malformed"},
			"a draft":                    {func(p *PullRequest) { p.IsDraft = true }, "disposition_conflict"},
			"closed":                     {func(p *PullRequest) { p.State = "closed" }, "disposition_conflict"},
			"already merged":             {func(p *PullRequest) { p.State = "merged" }, "disposition_conflict"},
			"moved while it was read": {func(p *PullRequest) {
				p.Verdict, p.Problems = "stale", []Problem{{Code: "candidate_moved"}}
			}, "merge_candidate_moved"},
			"unreadable": {func(p *PullRequest) { p.Verdict = "unknown" }, "not a refusal: "},
		} {
			pr := k.pr
			c.mutate(&pr)
			k.forge.by["owner/repo#5"] = pr
			_, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
			if got := refusalReason(err); !strings.HasPrefix(got, c.reason) {
				t.Fatalf("%s: judge = %v, want %s", name, err, c.reason)
			}
		}
		if k.count("SELECT COUNT(*) FROM dag_merge_checks") != 0 {
			t.Fatal("a case that could not be judged wrote a row")
		}
		k.forge.err = context.DeadlineExceeded
		if _, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{}); err == nil || k.count("SELECT COUNT(*) FROM dag_merge_checks") != 0 {
			t.Fatalf("a forge that cannot be reached = %v", err)
		}
		k.forge.err = nil
		for name, c := range map[string]struct {
			actor, node string
			in          JudgeInput
			reason      string
		}{
			"another task":          {"intruder", "I", JudgeInput{}, "scope_role_mismatch"},
			"another pull request":  {"parent", "I", JudgeInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 9}}, "disposition_conflict"},
			"a node with no branch": {"parent", "K", JudgeInput{}, "disposition_conflict"},
			"a node with no result": {"parent", "D", JudgeInput{}, "disposition_conflict"},
			"a node the plan lacks": {"parent", "Z", JudgeInput{}, "unregistered_scope"},
		} {
			if _, err := k.sched.Judge(context.Background(), "g", c.node, c.actor, c.in); refusalReason(err) != c.reason {
				t.Fatalf("%s: judge = %v, want %s", name, err, c.reason)
			}
		}
	})
}

func mustActive(t *testing.T, f *fixture, plan, node string) Acceptance {
	t.Helper()
	ctx := context.Background()
	a, found, err := loadActiveAcceptance(ctx, f.s.Q(ctx), plan, node)
	if err != nil || !found {
		t.Fatalf("no active acceptance of %s: %v", node, err)
	}
	return a
}

// B-13: a judgement is evidence the relay reads again, and a row that no longer digests to what was recorded is neither history nor a head: the edges built on it are blocked, and the next
// judgement refuses instead of counting retries on a falsified record.
func TestEvidenceRowTamperIsBlocked(t *testing.T) {
	k := newJudgeKit(t)
	repo := k.repo
	k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addEdge("id", "I", "D", "artifact_verified", doc{"pins_code_head": true, "target_repository": repo.path, "target_base_ref": "dev"}))
	k.integrate(accepted{Acceptance: mustActive(t, k.fixture, "g", "I"), Event: "evt-rel-g-I"}, repo.path, "dev", true, true)
	k.judge()
	if st := k.status("g", "id"); !st.Satisfied {
		t.Fatalf("before the tamper the edge is %+v", st)
	}
	k.exec("UPDATE dag_merge_checks SET evidence_json = replace(evidence_json, 'success', 'failure')")
	if st := k.status("g", "id"); st.Satisfied || st.Reason != BlockedEvidenceMismatch {
		t.Fatalf("after the tamper the edge is %+v", st)
	}
	before := k.count("SELECT COUNT(*) FROM dag_merge_checks")
	if _, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{}); refusalReason(err) != "revision_mismatch" {
		t.Fatalf("judge on a falsified history = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM dag_merge_checks") != before {
		t.Fatal("a judgement was written over a falsified history")
	}
}

// D-12 is a limit of a head of a pull request, not of an acceptance: when the node is accepted again at the same head (a new report, an explicit supersede) the head does not get its
// retry back, and the acceptance in force carries the eviction, so the reading blocks it from its own history.
func TestEvictionSurvivesAFreshAcceptanceOfTheSameHead(t *testing.T) {
	k := newJudgeKit(t)
	k.setChecks("A:1:1:failure", "B:2:1:success")
	k.judge()
	k.setChecks("A:1:2:failure", "B:2:1:success")
	if r := k.judge(); r.Outcome != OutcomeEvicted {
		t.Fatalf("second failure = %+v", r)
	}
	k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE node_id = 'I'")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: k.feature, PR: 5, Forge: "owner/repo", Repository: k.repo.path, Suffix: "b"})
	k.setChecks("A:1:3:success", "B:2:1:success")
	again := k.judge()
	if again.Outcome != OutcomeEvicted || again.Round != 2 {
		t.Fatalf("the same head under a new acceptance = %+v", again)
	}
	if n := k.read("g").node("I"); n.Reason != BlockedEvicted {
		t.Fatalf("I = %+v, want blocked:evicted from the acceptance's own history", n)
	}
	// and once it is carried, asking again only restates it, with the sequence of the acceptance's own row
	if rest := k.judge(); rest.Outcome != OutcomeEvicted || !rest.Replayed || rest.CheckSeq != again.CheckSeq {
		t.Fatalf("asking again = %+v, the carried row is %d", rest, again.CheckSeq)
	}
	var own int
	if err := k.s.DB.QueryRow("SELECT COUNT(*) FROM dag_merge_checks c JOIN dag_acceptances a ON a.acceptance_id = c.acceptance_id WHERE a.state = 'active' AND c.check_seq = ? AND c.outcome = 'evicted'", again.CheckSeq).Scan(&own); err != nil || own != 1 {
		t.Fatalf("no row of the acceptance in force has the sequence %d (%d, %v)", again.CheckSeq, own, err)
	}
	// a different head of the same pull request is a different matter: it needs its own acceptance, and starts with its own retry
	if k.count("SELECT COUNT(*) FROM dag_merge_checks WHERE outcome = 'evicted'") != 2 {
		t.Fatalf("history = %v", k.history())
	}
}

// Two nodes that were accepted at one commit are two pull requests of two relationships: a turn that is open for the first is not an answer for the second.
func TestMergeRequestIsNotAnsweredByAnotherNodesTurnAtTheSameHead(t *testing.T) {
	k := newJudgeKit(t)
	k.declare("g", "D", "d.txt")
	k.acceptNode("g", "D", acceptOpts{HeadSHA: k.feature, PR: 6, Forge: "owner/repo", Repository: k.repo.path})
	pr := k.pr
	pr.Number = 6
	k.forge.by["owner/repo#6"] = pr
	first, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
	if err != nil || !first.Eligible() || turn["relationshipId"] != "rel-g-I" {
		t.Fatalf("I = %v %+v %v", err, first, turn)
	}
	second, other, err := k.sched.RequestMergeTurn(context.Background(), "g", "D", "parent", MergeRequestInput{Host: "host"})
	if refusalReason(err) != "disposition_conflict" || other != nil || !second.Eligible() || !strings.Contains(err.Error(), "rel-g-I") {
		t.Fatalf("D at the same head = %v %+v %v", err, second, other)
	}
}

// A relationship the registry replaced (archived and superseded) is not a holder of a merge turn, and a pause that lands while the pull request is read is seen under the lock.
func TestMergeRequestRefusesASupersededRelationship(t *testing.T) {
	k := newJudgeKit(t)
	k.exec("PRAGMA foreign_keys = OFF")
	k.exec("UPDATE relationships SET status = 'archived', superseded_by = 'rel-successor' WHERE relationship_id = 'rel-g-I'")
	if _, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"}); refusalReason(err) != "relationship_not_active" || turn != nil {
		t.Fatalf("request = %v %v", err, turn)
	}
	if _, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{}); refusalReason(err) != "relationship_not_active" {
		t.Fatalf("judge = %v", err)
	}
	if k.count("SELECT COUNT(*) FROM merge_turns") != 0 || k.count("SELECT COUNT(*) FROM dag_merge_checks") != 0 {
		t.Fatal("something was written for a superseded relationship")
	}
}

type tipStub struct{ sha string }

func (t *tipStub) Tip(_ context.Context, repository, base string) (mergeturn.Tip, error) {
	return mergeturn.Tip{SHA: t.sha, Source: "stub", Reference: "refs/heads/" + base, Repository: repository}, nil
}

// The base tip a judgement was made against is part of what it says: the same outcome against a tip that moved is a new row, so the history never claims the old tip.
func TestMergeJudgementRecordsTheTipItWasMadeAgainst(t *testing.T) {
	k := newJudgeKit(t)
	older := k.feature // a tip the head contains: the head itself
	tip := &tipStub{sha: k.repo.git("rev-parse", "dev")}
	k.sched.Tips = tip
	first := k.judge()
	tip.sha = older
	second := k.judge()
	if !first.Eligible() || !second.Eligible() || second.Replayed || second.CheckSeq != 2 || second.BaseTipSHA != older {
		t.Fatalf("first %+v second %+v", first, second)
	}
	var recorded string
	if err := k.s.DB.QueryRow("SELECT base_tip_sha FROM dag_merge_checks WHERE check_seq = 2").Scan(&recorded); err != nil || recorded != older {
		t.Fatalf("recorded tip = %s %v", recorded, err)
	}
}

// The forge does not tell owner/name apart by case and a pull request can be replaced by another from the same commit: neither gives a head the retry it used up.
func TestEvictionIsOfTheCommitNotOfTheSpellingOrThePullRequest(t *testing.T) {
	k := newJudgeKit(t)
	k.setChecks("A:1:1:failure", "B:2:1:success")
	k.judge()
	k.setChecks("A:1:2:failure", "B:2:1:success")
	if r := k.judge(); r.Outcome != OutcomeEvicted {
		t.Fatalf("second failure = %+v", r)
	}
	k.exec("UPDATE dag_acceptances SET state = 'superseded' WHERE node_id = 'I'")
	k.acceptNode("g", "I", acceptOpts{HeadSHA: k.feature, PR: 9, Forge: "OWNER/REPO", Repository: k.repo.path, Suffix: "c"})
	pr := k.pr
	pr.Number, pr.Repository = 9, "OWNER/REPO"
	pr.Checks = []Check{{Name: "A", RunID: "1", Attempt: 3, Conclusion: "success", HeadSHA: k.feature}, {Name: "B", RunID: "2", Attempt: 1, Conclusion: "success", HeadSHA: k.feature}}
	k.forge.by["OWNER/REPO#9"] = pr
	if r := k.judge(); r.Outcome != OutcomeEvicted {
		t.Fatalf("the same commit under another spelling and another pull request = %+v", r)
	}
}

// Everything the judgement rested on is read again in the transaction that creates the turn: a pause that lands between the two creates neither a turn nor a grant.
func TestMergeRequestReadsAgainWhereItWrites(t *testing.T) {
	k := newJudgeKit(t)
	k.sched.testBetweenJudgeAndAsk = func() { k.exec("UPDATE relationships SET status = 'paused'") }
	if _, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"}); refusalReason(err) != "relationship_not_active" || turn != nil {
		t.Fatalf("request = %v %v", err, turn)
	}
	if k.count("SELECT COUNT(*) FROM merge_turns") != 0 || k.count("SELECT COUNT(*) FROM merge_turn_ledger") != 0 {
		t.Fatal("a turn or a ledger entry was created for a paused relationship")
	}
	k.exec("UPDATE relationships SET status = 'active'")
	k.sched.testBetweenJudgeAndAsk = func() { k.exec("UPDATE canonical_criteria SET set_digest = ?", dig("registered meanwhile")) }
	if _, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"}); refusalReason(err) != "criteria_set_changed" || turn != nil {
		t.Fatalf("request after the criteria moved = %v %v", err, turn)
	}
	if k.count("SELECT COUNT(*) FROM merge_turns") != 0 {
		t.Fatal("a turn was created for criteria that moved")
	}
}

// The judgement the turn rests on is checked again in the transaction that creates the turn, all of it: a predecessor edge that appears, a falsified record of the judgement and an
// integrity failure of the lane in the middle of writing a turn each leave no turn, no grant and no half-written ledger.
func TestMergeRequestCheckedAgainInTheTransactionThatCreatesTheTurn(t *testing.T) {
	ask := func(k *judgeKit) error {
		_, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
		if err != nil && turn != nil {
			t.Fatalf("a refusal came with a turn: %v", turn)
		}
		return err
	}
	nothing := func(k *judgeKit) {
		t.Helper()
		if k.count("SELECT COUNT(*) FROM merge_turns") != 0 || k.count("SELECT COUNT(*) FROM merge_turn_ledger") != 0 {
			t.Fatal("something of a turn was written")
		}
	}
	t.Run("a predecessor appears", func(t *testing.T) {
		k := newJudgeKit(t)
		k.sched.testBetweenJudgeAndAsk = func() {
			k.putPlan("g", int(k.snapshot("g").Revision), "g-r2", addEdge("di", "D", "I", "integrated", doc{"target_repository": k.repo.path, "target_base_ref": "dev"}))
			k.acceptNode("g", "D", acceptOpts{HeadSHA: strings.Repeat("3", 40), PR: 6, Forge: "owner/repo", Repository: k.repo.path})
		}
		if err := ask(k); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "has not landed") {
			t.Fatalf("request = %v", err)
		}
		nothing(k)
	})
	t.Run("the record of the judgement is falsified", func(t *testing.T) {
		k := newJudgeKit(t)
		k.sched.testBetweenJudgeAndAsk = func() {
			k.exec("UPDATE dag_merge_checks SET evidence_json = replace(evidence_json, 'success', 'failure')")
		}
		if err := ask(k); refusalReason(err) != "revision_mismatch" {
			t.Fatalf("request = %v", err)
		}
		nothing(k)
	})
	t.Run("the lane fails its own integrity check after writing part of the turn", func(t *testing.T) {
		k := newJudgeKit(t)
		target, err := mergeturn.TargetKey(k.repo.path, "dev")
		if err != nil {
			t.Fatal(err)
		}
		turn := mergeturn.TurnID(target, "parent", 1)
		// an entry of the ledger already holds the identity the new turn's claim writes, as something else
		k.exec("PRAGMA foreign_keys = OFF")
		k.exec("INSERT INTO merge_turn_ledger (entry_id, turn_id, kind, from_state, to_state, evidence_kind, actor_task_id, evidence, idempotency_key, recorded_at) VALUES (?, ?, 'transition', NULL, 'holding', 'claim', 'someone-else', 'something else', 'request:1', 't')",
			registry.CoordinationID("mte", turn, "request:1"), turn)
		before := k.count("SELECT COUNT(*) FROM merge_turn_ledger")
		if err := ask(k); refusalReason(err) != "merge_evidence_required" {
			t.Fatalf("request = %v", err)
		}
		if k.count("SELECT COUNT(*) FROM merge_turns") != 0 || k.count("SELECT COUNT(*) FROM merge_turn_ledger") != before {
			t.Fatal("a half-written turn was committed")
		}
	})
}

// A reading that was in flight while a newer one was judged is older than the history and counts for nothing: an old failure that arrives after the retry passed neither evicts the head
// nor takes back the eligibility. Nothing is written and the caller reads again.
func TestAnOlderReadingCannotOverwriteANewerJudgement(t *testing.T) {
	k := newJudgeKit(t)
	k.setChecks("A:1:1:failure", "B:2:1:success")
	k.judge()
	old := k.forge.by["owner/repo#5"]
	// while this judgement's read of the pull request is in flight, another judgement reads the retry's passing attempt and records it
	k.forge.onRead = func() {
		k.setChecks("A:1:2:success", "B:2:1:success")
		if r := k.judge(); !r.Eligible() {
			t.Errorf("the nested judgement = %+v", r)
		}
		k.forge.by["owner/repo#5"] = old
	}
	_, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{})
	if refusalReason(err) != "merge_candidate_moved" {
		t.Fatalf("the older reading = %v", err)
	}
	if got := strings.Join(k.history(), " "); got != "retry_same_sha:1 eligible:2" {
		t.Fatalf("history = %s", got)
	}
}

// A refusal the lane records as a conflict is an answer and its row is kept, and a repeat of the same contest updates the row (a later time) instead of adding one: that update is kept too.
func TestMergeRequestKeepsTheLanesRecordOfARepeatedContest(t *testing.T) {
	k := newJudgeKit(t)
	// the project's binding changed hands after the relationship was made: the lane refuses the holder and records the contest
	k.exec("UPDATE scope_bindings SET task_id = 'someone-else' WHERE scope_kind = 'project' AND scope_key = 'P-TEST'")
	at := func() string {
		var at string
		if err := k.s.DB.QueryRow("SELECT at FROM coordination_conflicts WHERE reason = 'scope_role_mismatch' AND domain = 'merge_target'").Scan(&at); err != nil {
			t.Fatalf("no recorded contest: %v", err)
		}
		return at
	}
	ask := func() error {
		_, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"})
		if turn != nil {
			t.Fatalf("a turn was granted to a project that another task holds: %v", turn)
		}
		return err
	}
	if err := ask(); refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("first = %v", err)
	}
	first := at()
	if err := ask(); refusalReason(err) != "scope_role_mismatch" {
		t.Fatalf("second = %v", err)
	}
	if second := at(); second <= first {
		t.Fatalf("the repeated contest left its row at %s, was %s", second, first)
	}
	if k.count("SELECT COUNT(*) FROM merge_turns") != 0 {
		t.Fatal("a turn exists")
	}
}

// Everything is computed from the newest reading of each run. A listing that still carries a failed first attempt beside the passing second one is eligible and is the same judgement read
// again; a failure that a judgement could not count (a check was still running beside it) is not the first failure of a retry round; a reading of a run that is older than the one already
// judged, in the same attempt, is discarded as well.
func TestJudgementsAreComputedFromTheNewestReadingOfEachRun(t *testing.T) {
	t.Run("a listing with two attempts of a run", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure", "A:1:2:success", "B:2:1:success")
		first := k.judge()
		if !first.Eligible() {
			t.Fatalf("first = %+v", first)
		}
		if again := k.judge(); !again.Eligible() || !again.Replayed {
			t.Fatalf("the same listing again = %+v", again)
		}
		if _, turn, err := k.sched.RequestMergeTurn(context.Background(), "g", "I", "parent", MergeRequestInput{Host: "host"}); err != nil || turn == nil {
			t.Fatalf("the turn for the same listing = %v %v", err, turn)
		}
	})
	t.Run("failures that were not counted do not open a retry round", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure", "B:2:1:")
		if r := k.judge(); r.Outcome != OutcomeChecksPending || len(r.FailedRequired) != 0 {
			t.Fatalf("A red beside a running B = %+v", r)
		}
		k.setChecks("A:1:2:success", "B:2:1:")
		k.judge()
		k.setChecks("A:1:3:failure", "B:2:1:success")
		first := k.judge()
		if first.Outcome != OutcomeRetrySameSHA || first.Round != 1 {
			t.Fatalf("the first counted failure = %+v", first)
		}
		if again := k.judge(); again.Outcome != OutcomeRetrySameSHA || !again.Replayed {
			t.Fatalf("the first counted failure read again = %+v", again)
		}
	})
	t.Run("an older completion of a run in the same attempt", func(t *testing.T) {
		k := newJudgeKit(t)
		k.setChecks("A:1:1:failure:2026-10-02T00:00:01Z", "B:2:1:success")
		k.judge()
		// the run was reset and completed again at another time, and passed
		k.setChecks("A:1:1:success:2026-10-02T00:05:00Z", "B:2:1:success")
		if r := k.judge(); !r.Eligible() {
			t.Fatalf("the reset run passed = %+v", r)
		}
		// a delayed reading of the earlier completion arrives
		k.setChecks("A:1:1:failure:2026-10-02T00:00:01Z", "B:2:1:success")
		if _, err := k.sched.Judge(context.Background(), "g", "I", "parent", JudgeInput{}); refusalReason(err) != "merge_candidate_moved" {
			t.Fatalf("the older completion = %v", err)
		}
		if got := strings.Join(k.history(), " "); got != "retry_same_sha:1 eligible:2" {
			t.Fatalf("history = %s", got)
		}
	})
}

// Names and run identities are opaque texts: two runs whose joined texts would be equal are two runs.
func TestRunKeysAreNotJoinedTexts(t *testing.T) {
	head := strings.Repeat("a", 40)
	a := Check{Name: "A|check-run:11|status:A", RunID: "check-run:11", HeadSHA: head, Attempt: 1, Conclusion: "failure"}
	b := Check{Name: "A|check-run:11", RunID: "status:A|check-run:11", HeadSHA: head, Attempt: 1, Conclusion: "failure"}
	if got := reduceRuns(head, []Check{a, b}); len(got) != 2 {
		t.Fatalf("reduced to %d runs: %v", len(got), got)
	}
	fa, fb := failure{Name: a.Name, Run: a.RunID, Attempt: 1}, failure{Name: b.Name, Run: b.RunID, Attempt: 1}
	if fa == fb || fa.key() == fb.key() {
		t.Fatal("two different failures compare equal")
	}
	round, err := parseFailures(failuresJSON([]failure{fa, fb}))
	if err != nil || len(round) != 2 || round[0] == round[1] {
		t.Fatalf("stored and read back = %v %v", round, err)
	}
}
