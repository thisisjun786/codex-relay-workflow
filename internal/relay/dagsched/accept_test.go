package dagsched

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var verifier = VerifierRule{SkillsDigest: "skills-1", Model: "gpt-5", Effort: "high"}

func (k *releaseKit) accept(plan, node string, in AcceptInput) (AcceptResult, error) {
	k.t.Helper()
	if in.RuleVersion == (VerifierRule{}) {
		in.RuleVersion = verifier
	}
	return k.sched.Accept(context.Background(), plan, node, "parent", in)
}

func (k *releaseKit) acceptCount() int { return k.count("SELECT COUNT(*) FROM dag_acceptances") }

// Criterion c5: an acceptance is written only when P-AV-1 holds now. Each case breaks one link of the chain that makes a result accepted: the baseline accepts, every other case is refused with the
// relay's own reason and writes nothing.
func TestAcceptRequiresPAV1(t *testing.T) {
	cases := []struct {
		name   string
		reason string // "" = accepted
		mutate func(k *releaseKit, r accepted)
		input  AcceptInput
		actor  string
	}{
		{name: "baseline"},
		{name: "the event the parent names is the head", input: AcceptInput{Event: "evt-rel-rp-A"}},
		{name: "a report that was only staged", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) {
			k.exec("UPDATE events SET stage = 'staged' WHERE event_id = ?", r.Event)
		}},
		{name: "a suppressed report", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) {
			k.exec("UPDATE events SET suppressed_reason = 'late' WHERE event_id = ?", r.Event)
		}},
		{name: "no acknowledgement", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) { k.exec("DELETE FROM acks") }},
		{name: "an acknowledgement the host did not verify", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE acks SET verified = 'unverified'") }},
		{name: "an acknowledgement that was not accepted", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE acks SET accepted = 0") }},
		{name: "evidence of the unverified tier", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE ack_evidence SET tier = 'unverified'") }},
		{name: "no ruling", reason: "disposition_conflict", mutate: func(k *releaseKit, r accepted) { k.exec("DELETE FROM verdicts") }},
		{name: "a ruling that asks for changes", reason: "disposition_conflict", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE verdicts SET verdict = 'needs_changes'") }},
		{name: "a ruling that is no longer current", reason: "superseded_revision", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE verdict_context SET currency = 'stale'") }},
		{name: "a ruling about another head", reason: "superseded_revision", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE verdict_context SET head_event_id = 'someone-else'") }},
		{name: "criteria registered again after the ruling", reason: "criteria_set_changed", mutate: func(k *releaseKit, r accepted) {
			k.exec("UPDATE canonical_criteria SET set_digest = ?", dig("registered again"))
		}},
		{name: "legacy verification", reason: "disposition_conflict", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE verification_mode SET mode = 'legacy'") }},
		{name: "the plan changed the node's criteria", reason: "criteria_set_changed", mutate: func(k *releaseKit, r accepted) {
			node := nodeDoc("A", dag.NodeNonPR)
			node["criteria_set_digest"] = dig("new plan criteria")
			k.putPlan("rp", 1, "rp-r1b", doc{"op": dag.OpUpdateNode, "node": node})
		}},
		{name: "another task's acceptance", reason: "scope_role_mismatch", actor: "intruder"},
		{name: "a paused relationship", reason: "relationship_not_active", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE relationships SET status = 'paused'") }},
		{name: "an archived relationship", reason: "relationship_not_active", mutate: func(k *releaseKit, r accepted) { k.exec("UPDATE relationships SET status = 'archived'") }},
		{name: "an event that is not the head", reason: "stale_generation", input: AcceptInput{Event: "evt-elsewhere"}},
		{name: "a second head with no lineage (ambiguous)", reason: "revision_ambiguous", mutate: func(k *releaseKit, r accepted) {
			k.exec("INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash, outcome, producer, turn_thread_id, turn_id, turn_status, receipt, stage, first_seen_at, last_seen_at)"+
				" VALUES ('evt-second', ?, 1, ?, 'ready_for_review', 'child', 'child-A', 'turn-2', 'completed', '{}', 'final', 'z', 'z')", r.Acceptance.RelationshipID, dig("a second revision"))
		}},
		{name: "no report at all", reason: "not_acknowledged", mutate: func(k *releaseKit, r accepted) { k.exec("DELETE FROM events") }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			r := k.reportNode("rp", "A", acceptOpts{})
			if c.mutate != nil {
				c.mutate(k, r)
			}
			actor := c.actor
			if actor == "" {
				actor = "parent"
			}
			in := c.input
			in.RuleVersion = verifier
			res, err := k.sched.Accept(context.Background(), "rp", "A", actor, in)
			if c.reason == "" {
				if err != nil || res.AcceptanceID == "" || res.Replayed || k.acceptCount() != 1 {
					t.Fatalf("accept = %v %+v (%d rows)", err, res, k.acceptCount())
				}
				return
			}
			if refusalReason(err) != c.reason {
				t.Fatalf("accept = %v, want %s", err, c.reason)
			}
			if k.acceptCount() != 0 {
				t.Fatal("a refused acceptance wrote a row")
			}
			if n := k.read("rp").node("B"); n.Reason != WaitEdge("ab") {
				t.Fatalf("B = %+v: a refused acceptance opened the edge", n)
			}
		})
	}
}

// The acceptance is the node's value (contract 4.3): it opens the edge, it is idempotent per output, and a new output replaces it only explicitly.
func TestAcceptIsIdempotentAndSupersedesExplicitly(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.reportNode("rp", "A", acceptOpts{})
	first, err := k.accept("rp", "A", AcceptInput{})
	if err != nil || first.AcceptanceID == "" {
		t.Fatalf("first = %v %+v", err, first)
	}
	row := k.s.DB.QueryRow("SELECT accepted_by_task_id, ack_tier, verdict_turn_id, rule_version_json, manifest_digest, coordinator_epoch, state FROM dag_acceptances WHERE acceptance_id = ?", first.AcceptanceID)
	var by, tier, turn, rule, manifest, state string
	var epoch int
	if err := row.Scan(&by, &tier, &turn, &rule, &manifest, &epoch, &state); err != nil || by != "parent" || tier != "host_read" || turn != "verdict-turn" || epoch != 0 || state != "active" || !strings.Contains(rule, "skills-1") || manifest == "" {
		t.Fatalf("row = %v %v %v %v %v %v %v", by, tier, turn, rule, manifest, epoch, state)
	}
	if n := k.read("rp").node("B"); n.Disposition != DispReady && n.Reason != "" {
		t.Fatalf("B = %+v, want it released by the acceptance's artifacts", n)
	}
	again, err := k.accept("rp", "A", AcceptInput{})
	if err != nil || !again.Replayed || again.AcceptanceID != first.AcceptanceID || k.acceptCount() != 1 {
		t.Fatalf("the same output again = %v %+v (%d rows)", err, again, k.acceptCount())
	}
	// a new output (a revision of the report that supersedes the first) replaces the active acceptance only explicitly
	k.supersedeReport(first.RelationshipID, "A", "rp", "second")
	if _, err := k.accept("rp", "A", AcceptInput{}); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a new output without supersedes = %v", err)
	}
	if _, err := k.accept("rp", "A", AcceptInput{Supersedes: dig("another acceptance")}); refusalReason(err) != "disposition_conflict" {
		t.Fatalf("a new output superseding another acceptance = %v", err)
	}
	if k.acceptCount() != 1 {
		t.Fatal("a refused replacement wrote a row")
	}
	second, err := k.accept("rp", "A", AcceptInput{Supersedes: first.AcceptanceID})
	if err != nil || second.SupersededID != first.AcceptanceID || second.AcceptanceID == first.AcceptanceID {
		t.Fatalf("explicit supersede = %v %+v", err, second)
	}
	if k.count("SELECT COUNT(*) FROM dag_acceptances WHERE state = 'active'") != 1 || k.count("SELECT COUNT(*) FROM dag_acceptances WHERE state = 'superseded' AND acceptance_id = ?", first.AcceptanceID) != 1 {
		t.Fatal("the replaced acceptance is not superseded or two are active")
	}
	var chain string
	if err := k.s.DB.QueryRow("SELECT supersedes_acceptance_id FROM dag_acceptances WHERE acceptance_id = ?", second.AcceptanceID).Scan(&chain); err != nil || chain != first.AcceptanceID {
		t.Fatalf("supersedes_acceptance_id = %q %v", chain, err)
	}
}

// E-11: the same output ruled again under re-registered criteria revalidates the same acceptance: one acceptance row, a history of revalidations, no new generation and no new child.
func TestAcceptRevalidatesAfterCriteriaChange(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.reportNode("rp", "A", acceptOpts{})
	first, err := k.accept("rp", "A", AcceptInput{})
	if err != nil {
		t.Fatal(err)
	}
	node, _ := nodeOf(k.snapshot("rp"), "A")
	original := node.CriteriaSetDigest
	// the criteria are registered again as C and the plan follows; the output is ruled again at C
	c := dig("criteria C")
	retarget := func(digest string) {
		n := relNode("A", dag.NodeNonPR)
		n["criteria_set_digest"] = digest
		k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r-"+digest[:8], doc{"op": dag.OpUpdateNode, "node": n})
		k.exec("UPDATE canonical_criteria SET set_digest = ?", digest)
		k.exec("UPDATE verdict_context SET set_digest = ?", digest)
	}
	retarget(c)
	if st := k.status("rp", "ab"); st.Satisfied {
		t.Fatalf("the edge is satisfied on criteria the acceptance does not stand on: %+v", st)
	}
	res, err := k.accept("rp", "A", AcceptInput{})
	if err != nil || !res.Revalidated || res.AcceptanceID != first.AcceptanceID {
		t.Fatalf("revalidation = %v %+v", err, res)
	}
	if st := k.status("rp", "ab"); !st.Satisfied {
		t.Fatalf("after revalidation the edge is %+v", st)
	}
	// criteria B again, ruled again: a third state, still one acceptance
	retarget(original)
	if res, err := k.accept("rp", "A", AcceptInput{}); err != nil || !res.Revalidated {
		t.Fatalf("second revalidation = %v %+v", err, res)
	}
	if again, err := k.accept("rp", "A", AcceptInput{}); err != nil || !again.Replayed {
		t.Fatalf("repeat = %v %+v", err, again)
	}
	if k.acceptCount() != 1 || k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations") != 2 {
		t.Fatalf("%d acceptances, %d revalidations", k.acceptCount(), k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations"))
	}
	if k.count("SELECT COUNT(*) FROM dag_node_executions") != 1 || k.count("SELECT COUNT(*) FROM relationships") != 1 {
		t.Fatal("a revalidation opened a generation or a child")
	}
	var seq1, seq2 string
	if err := k.s.DB.QueryRow("SELECT criteria_set_digest FROM dag_acceptance_revalidations WHERE reval_seq = 1").Scan(&seq1); err != nil || seq1 != c {
		t.Fatalf("revalidation 1 = %s %v", seq1, err)
	}
	if err := k.s.DB.QueryRow("SELECT criteria_set_digest FROM dag_acceptance_revalidations WHERE reval_seq = 2").Scan(&seq2); err != nil || seq2 != original {
		t.Fatalf("revalidation 2 = %s %v", seq2, err)
	}
}

// Criterion c5: for an implementation node the accepted head is the pull request head the relay read from the forge, never a child's statement; the forge row follows the acceptance in
// the same transaction (its foreign key is immediate) and a failure of either leaves neither.
func TestAcceptCodeNodePinsTheForgeHead(t *testing.T) {
	setup := func(t *testing.T) *releaseKit {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.declare("rp", "I", "i.go")
		k.reportNode("rp", "I", acceptOpts{})
		return k
	}
	pr := func(head string) *PRRef { return &PRRef{Repository: "owner/repo", Number: 7} }
	t.Run("the head is the forge's", func(t *testing.T) {
		k := setup(t)
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
		res, err := k.accept("rp", "I", AcceptInput{PullRequest: pr(head1)})
		if err != nil || res.HeadSHA != head1 || res.EvidenceDigest == "" {
			t.Fatalf("accept = %v %+v", err, res)
		}
		var head, repository, forgeRepo string
		var number, forgeNumber int64
		if err := k.s.DB.QueryRow("SELECT head_sha, repository, pr_number FROM dag_acceptances").Scan(&head, &repository, &number); err != nil || head != head1 || repository != "owner/repo" || number != 7 {
			t.Fatalf("acceptance = %s %s %d %v", head, repository, number, err)
		}
		if err := k.s.DB.QueryRow("SELECT forge_repository, pr_number FROM dag_acceptance_forge").Scan(&forgeRepo, &forgeNumber); err != nil || forgeRepo != "owner/repo" || forgeNumber != 7 {
			t.Fatalf("forge row = %s %d %v", forgeRepo, forgeNumber, err)
		}
	})
	for name, c := range map[string]struct {
		pr     PullRequest
		input  AcceptInput
		reason string
	}{
		"a draft":                               {pr: func() PullRequest { p := openPR("owner/repo", 7, head1); p.IsDraft = true; return p }(), input: AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}, reason: "disposition_conflict"},
		"closed":                                {pr: func() PullRequest { p := openPR("owner/repo", 7, head1); p.State = "closed"; return p }(), input: AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}, reason: "disposition_conflict"},
		"no pull request named":                 {pr: openPR("owner/repo", 7, head1), input: AcceptInput{}, reason: "malformed_receipt"},
		"a repository without an owner":         {pr: openPR("owner/repo", 7, head1), input: AcceptInput{PullRequest: &PRRef{Repository: "repo", Number: 7}}, reason: "not a refusal"},
		"the candidate moved while it was read": {pr: PullRequest{Repository: "owner/repo", Number: 7, HeadSHA: head1, Verdict: "stale", Problems: []Problem{{Code: "candidate_moved"}}}, input: AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}, reason: "merge_candidate_moved"},
	} {
		t.Run(name, func(t *testing.T) {
			k := setup(t)
			k.forge.by["owner/repo#7"] = c.pr
			_, err := k.accept("rp", "I", c.input)
			if got := refusalReason(err); !strings.HasPrefix(got, c.reason) {
				t.Fatalf("accept = %v, want %s", err, c.reason)
			}
			if k.acceptCount() != 0 {
				t.Fatal("a refused acceptance wrote a row")
			}
		})
	}
	t.Run("a non_pr node names no pull request", func(t *testing.T) {
		k := newReleaseKit(t)
		releasePlan(k.fixture, "rp")
		k.reportNode("rp", "A", acceptOpts{})
		if _, err := k.accept("rp", "A", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}); refusalReason(err) != "malformed_receipt" {
			t.Fatalf("accept = %v", err)
		}
	})
	t.Run("the forge row cannot be written: nothing is", func(t *testing.T) {
		k := setup(t)
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
		k.exec("CREATE TRIGGER refuse_forge BEFORE INSERT ON dag_acceptance_forge BEGIN SELECT RAISE(ABORT, 'no forge row'); END")
		if _, err := k.accept("rp", "I", AcceptInput{PullRequest: pr(head1)}); err == nil || !strings.Contains(err.Error(), "no forge row") {
			t.Fatalf("accept = %v", err)
		}
		if k.acceptCount() != 0 {
			t.Fatal("an acceptance was committed without its forge row")
		}
	})
}

// The slot of a node returns where the node's work ends: at the acceptance of a non_pr node. An operator who returned it first leaves the acceptance to commit without a second return.
func TestAcceptReleasesTheSlotOfANonPRNode(t *testing.T) {
	k := newReleaseKit(t)
	releasePlan(k.fixture, "rp")
	k.mustRelease("rp", "A")
	// the real release left a held slot; the child's report is seeded on its relationship
	held := func() int {
		return k.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", SlotSubjectKey("rp", "A"))
	}
	if held() != 1 {
		t.Fatalf("held = %d", held())
	}
	var relationship string
	if err := k.s.DB.QueryRow("SELECT relationship_id FROM dag_node_executions").Scan(&relationship); err != nil {
		t.Fatal(err)
	}
	k.seedReport(relationship, "A", "rp")
	res, err := k.accept("rp", "A", AcceptInput{})
	if err != nil || !res.SlotReleased || held() != 0 {
		t.Fatalf("accept = %v %+v held %d", err, res, held())
	}
	// operator first: the slot was returned with another reason; the acceptance still commits
	k2 := newReleaseKit(t)
	releasePlan(k2.fixture, "rp")
	k2.mustRelease("rp", "A")
	if err := k2.s.DB.QueryRow("SELECT relationship_id FROM dag_node_executions").Scan(&relationship); err != nil {
		t.Fatal(err)
	}
	if _, err := (&capacity.Capacity{Store: k2.s, Now: k2.clock}).Release(context.Background(), capacity.Release{SubjectKind: SlotSubjectKind, SubjectKey: SlotSubjectKey("rp", "A"), ReleasedBy: "parent", Reason: "operator"}); err != nil {
		t.Fatal(err)
	}
	k2.seedReport(relationship, "A", "rp")
	res, err = k2.accept("rp", "A", AcceptInput{})
	if err != nil || res.SlotReleased || res.AcceptanceID == "" {
		t.Fatalf("accept after the operator = %v %+v", err, res)
	}
}

// Contract 8.2: a result verified against a plan that moved is not accepted. The pull request is read before the transaction; a plan revision that changes the node while it is being read (its
// slice, or the criteria it is ruled against) must refuse the acceptance and write nothing.
func TestAcceptRefusesAPlanThatChangedWhileTheHeadWasRead(t *testing.T) {
	for name, change := range map[string]func(n doc){
		"the node's slice":    func(n doc) { n["issue_key"] = "CRW-OTHER" },
		"the node's criteria": func(n doc) { n["criteria_set_digest"] = dig("criteria of the new revision") },
	} {
		t.Run(name, func(t *testing.T) {
			k := newReleaseKit(t)
			releasePlan(k.fixture, "rp")
			k.declare("rp", "I", "i.go")
			k.reportNode("rp", "I", acceptOpts{})
			k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
			read := k.sched.PRs
			k.sched.PRs = func(ctx context.Context, repository string, number int64) (PullRequest, error) {
				pr, err := read(ctx, repository, number)
				node := relNode("I", dag.NodeImplementation)
				change(node)
				k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-moved", doc{"op": dag.OpUpdateNode, "node": node})
				return pr, err
			}
			_, err := k.accept("rp", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}})
			if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "plan changed") {
				t.Fatalf("accept = %v, want the plan-changed refusal", err)
			}
			if k.acceptCount() != 0 {
				t.Fatal("an acceptance was written for a plan that moved")
			}
		})
	}
}

// acceptTargetCheckout is the absolute path of a local checkout that a plan names as an edge target. Nothing reads it: an acceptance looks at the pull request, never at a repository on disk.
const acceptTargetCheckout = "/work/checkouts/repo"

// acceptTargetPull is the pull request every acceptance of the tests below names; the forge shows it open at head1.
var acceptTargetPull = AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}}

// acceptTargetIntegrated is an edge from I that lands on target, and acceptTargetPinned one that pins I's code head to it.
func acceptTargetIntegrated(to, target string) doc {
	return addEdge("i"+strings.ToLower(to), "I", to, dag.EdgeIntegrated, doc{"target_repository": target})
}

func acceptTargetPinned(to, target string) doc {
	return addEdge("i"+strings.ToLower(to), "I", to, dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": target, "target_base_ref": "dev"})
}

// acceptTargetKit is a plan with the implementation nodes I and J and the non_pr nodes X and Y, the edges given, a verified report of I and the pull request owner/repo#7 open at head1.
func acceptTargetKit(t *testing.T, edges ...doc) *releaseKit {
	t.Helper()
	k := newReleaseKit(t)
	k.putPlan("rp", 0, "rp-r1", append([]doc{addRelNode("I", dag.NodeImplementation), addRelNode("J", dag.NodeImplementation), addRelNode("X", dag.NodeNonPR), addRelNode("Y", dag.NodeNonPR)}, edges...)...)
	k.reportNode("rp", "I", acceptOpts{})
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
	return k
}

// acceptTargetRefusal is the reason and the whole detail of a refusal, and the error as it is when it is none.
func acceptTargetRefusal(err error) (reason, detail string) {
	var refused *store.RefusedError
	if errors.As(err, &refused) {
		return refused.Reason, refused.Detail
	}
	if err != nil {
		return "not a refusal", err.Error()
	}
	return "", ""
}

func (k *releaseKit) acceptTargetForgeRows() int {
	return k.count("SELECT COUNT(*) FROM dag_acceptance_forge")
}

const (
	// acceptTargetRemedy is the refusal of an absolute path (decision 77): the forge is the target, the checkout belongs to dag-base-refresh. acceptTargetLandsOn is every other target that is not the forge.
	acceptTargetRemedy  = "the outgoing edges of I name local checkout %s as their target; an implementation node is accepted on its pull request's forge repository owner/repo. Set target_repository to owner/repo and use --checkout for dag-base-refresh when local Git objects are needed"
	acceptTargetLandsOn = "the outgoing edges of I land on %s and the pull request is in owner/repo"
)

// Decision 77: a new implementation acceptance names its pull request's forge repository as its integration target. A local checkout is a place to read Git objects, never an identity the merge lane works
// under, and a plan does not check how a target is spelled, so equality with the forge is the rule: a local checkout is refused with the way out, every other target that is not that repository is refused as
// landing elsewhere, and a refusal writes no acceptance and no forge row. Before the guard the local, relative, bare and malformed rows below were accepted; the other-case, leading-space and other-forge rows were already refused and stay as controls of exact equality.
func TestAcceptTargetIsTheForgeRepositoryOfThePullRequest(t *testing.T) {
	for _, c := range []struct {
		name   string
		edge   func(to, target string) doc
		target string
		detail string
	}{
		{"a local checkout an integrated edge lands on", acceptTargetIntegrated, acceptTargetCheckout, fmt.Sprintf(acceptTargetRemedy, acceptTargetCheckout)},
		{"a local checkout a code-pinned artifact edge names", acceptTargetPinned, acceptTargetCheckout, fmt.Sprintf(acceptTargetRemedy, acceptTargetCheckout)},
		{"a relative path", acceptTargetIntegrated, "../checkout", fmt.Sprintf(acceptTargetLandsOn, "../checkout")},
		{"a bare name", acceptTargetPinned, "checkout", fmt.Sprintf(acceptTargetLandsOn, "checkout")},
		{"a path with too many segments", acceptTargetIntegrated, "owner/repo/extra", fmt.Sprintf(acceptTargetLandsOn, "owner/repo/extra")},
		{"a URL", acceptTargetPinned, "https://example.com/owner/repo", fmt.Sprintf(acceptTargetLandsOn, "https://example.com/owner/repo")},
		{"a name without a repository", acceptTargetIntegrated, "owner/", fmt.Sprintf(acceptTargetLandsOn, "owner/")},
		{"the forge repository spelled in another case", acceptTargetIntegrated, "Owner/Repo", fmt.Sprintf(acceptTargetLandsOn, "Owner/Repo")},
		{"the forge repository with a space before it", acceptTargetPinned, " owner/repo", fmt.Sprintf(acceptTargetLandsOn, " owner/repo")},
		{"another forge repository", acceptTargetPinned, "owner/other", fmt.Sprintf(acceptTargetLandsOn, "owner/other")},
	} {
		t.Run(c.name, func(t *testing.T) {
			k := acceptTargetKit(t, c.edge("X", c.target))
			_, err := k.accept("rp", "I", acceptTargetPull)
			if reason, detail := acceptTargetRefusal(err); reason != "disposition_conflict" || detail != c.detail {
				t.Fatalf("accept = %s: %s\nwant disposition_conflict: %s", reason, detail, c.detail)
			}
			if k.acceptCount() != 0 || k.acceptTargetForgeRows() != 0 {
				t.Fatalf("a refused acceptance wrote %d acceptances and %d forge rows", k.acceptCount(), k.acceptTargetForgeRows())
			}
		})
	}
}

// What the guard leaves as it was: the forge's own repository on one or both kinds of edge, a terminal node (no outgoing edge, so the forge is the default), the refusal of several repositories (checked first,
// so a checkout beside a forge is that refusal), and a node with no pull request, whose acceptance never looks at a target.
func TestAcceptTargetKeepsTheRoutesItDoesNotGuard(t *testing.T) {
	accepts := func(name string, edges ...doc) {
		t.Run(name, func(t *testing.T) {
			k := acceptTargetKit(t, edges...)
			res, err := k.accept("rp", "I", acceptTargetPull)
			if err != nil || res.AcceptanceID == "" || res.Replayed || k.acceptCount() != 1 || k.acceptTargetForgeRows() != 1 {
				t.Fatalf("accept = %v %+v (%d acceptances, %d forge rows)", err, res, k.acceptCount(), k.acceptTargetForgeRows())
			}
			var repository, forge string
			if err := k.s.DB.QueryRow("SELECT a.repository, f.forge_repository FROM dag_acceptances a JOIN dag_acceptance_forge f ON f.acceptance_id = a.acceptance_id").Scan(&repository, &forge); err != nil || repository != "owner/repo" || forge != "owner/repo" {
				t.Fatalf("target %q, forge %q, %v", repository, forge, err)
			}
		})
	}
	accepts("an integrated edge on the forge repository", acceptTargetIntegrated("X", "owner/repo"))
	accepts("an integrated and a pinned edge on the same forge repository", acceptTargetIntegrated("X", "owner/repo"), acceptTargetPinned("Y", "owner/repo"))
	accepts("a terminal node")
	// an edge of another node is not a target of I's acceptance: the forge is
	accepts("a checkout named by another node's edge", addEdge("jy", "J", "Y", dag.EdgeArtifactVerified, doc{"pins_code_head": true, "target_repository": acceptTargetCheckout, "target_base_ref": "dev"}))
	t.Run("a checkout beside a forge repository is several repositories", func(t *testing.T) {
		k := acceptTargetKit(t, acceptTargetIntegrated("X", "owner/repo"), acceptTargetPinned("Y", acceptTargetCheckout))
		_, err := k.accept("rp", "I", acceptTargetPull)
		if reason, detail := acceptTargetRefusal(err); reason != "malformed_receipt" || detail != "the outgoing edges of I name more than one repository; one acceptance is judged against one" {
			t.Fatalf("accept = %s: %s", reason, detail)
		}
		if k.acceptCount() != 0 || k.acceptTargetForgeRows() != 0 {
			t.Fatal("a refused acceptance wrote a row")
		}
	})
	t.Run("a node with no pull request", func(t *testing.T) {
		// the plan's only integration target is a local checkout, and Y, which has no outgoing edge and no pull request, is accepted all the same
		k := acceptTargetKit(t, acceptTargetIntegrated("X", acceptTargetCheckout))
		k.reportNode("rp", "Y", acceptOpts{})
		res, err := k.accept("rp", "Y", AcceptInput{})
		if err != nil || res.AcceptanceID == "" || k.acceptCount() != 1 || k.acceptTargetForgeRows() != 0 || k.count("SELECT COUNT(*) FROM dag_acceptances WHERE repository IS NOT NULL OR pr_number IS NOT NULL") != 0 {
			t.Fatalf("accept = %v %+v (%d acceptances, %d forge rows)", err, res, k.acceptCount(), k.acceptTargetForgeRows())
		}
	})
}

// Acceptances recorded before the guard keep the target they stored. Replay and re-validation find the acceptance of the output and never ask for its target again, so a local one is neither rewritten nor
// refused. A replacement is a new acceptance: it is refused while the plan's edges still name the checkout, the refusal leaves the acceptance in force as it was (the replacement's update of its state is in the
// refused transaction), and it succeeds once the plan's edge lands on the forge. The rows of a legacy local acceptance are seeded as they were written then (acceptNode), not through Accept.
func TestAcceptTargetLeavesALegacyLocalAcceptanceAsItWas(t *testing.T) {
	legacy := func(t *testing.T) (*releaseKit, accepted) {
		k := newReleaseKit(t)
		k.putPlan("rp", 0, "rp-r1", addRelNode("I", dag.NodeImplementation), addRelNode("X", dag.NodeNonPR), acceptTargetIntegrated("X", acceptTargetCheckout))
		k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head1)
		return k, k.acceptNode("rp", "I", acceptOpts{HeadSHA: head1, PR: 7, Forge: "owner/repo", Repository: acceptTargetCheckout})
	}
	stored := func(k *releaseKit, id string) (repository, state string) {
		t := k.t
		t.Helper()
		if err := k.s.DB.QueryRow("SELECT repository, state FROM dag_acceptances WHERE acceptance_id = ?", id).Scan(&repository, &state); err != nil {
			t.Fatal(err)
		}
		return repository, state
	}
	t.Run("a replay keeps the stored checkout", func(t *testing.T) {
		k, old := legacy(t)
		res, err := k.accept("rp", "I", acceptTargetPull)
		if repository, state := stored(k, old.Acceptance.AcceptanceID); err != nil || !res.Replayed || res.AcceptanceID != old.Acceptance.AcceptanceID || repository != acceptTargetCheckout || state != "active" ||
			k.acceptCount() != 1 || k.acceptTargetForgeRows() != 1 {
			t.Fatalf("replay = %v %+v: stored %q %q, %d acceptances, %d forge rows", err, res, repository, state, k.acceptCount(), k.acceptTargetForgeRows())
		}
	})
	t.Run("a re-validation keeps the stored checkout", func(t *testing.T) {
		k, old := legacy(t)
		c := dig("criteria registered again")
		node := relNode("I", dag.NodeImplementation)
		node["criteria_set_digest"] = c
		k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", doc{"op": dag.OpUpdateNode, "node": node})
		k.exec("UPDATE canonical_criteria SET set_digest = ?", c)
		k.exec("UPDATE verdict_context SET set_digest = ?", c)
		res, err := k.accept("rp", "I", acceptTargetPull)
		if repository, state := stored(k, old.Acceptance.AcceptanceID); err != nil || !res.Revalidated || res.AcceptanceID != old.Acceptance.AcceptanceID || repository != acceptTargetCheckout || state != "active" ||
			k.acceptCount() != 1 || k.count("SELECT COUNT(*) FROM dag_acceptance_revalidations") != 1 {
			t.Fatalf("revalidation = %v %+v: stored %q %q, %d acceptances", err, res, repository, state, k.acceptCount())
		}
	})
	t.Run("a refused replacement leaves the acceptance active and writes nothing", func(t *testing.T) {
		k, old := legacy(t)
		k.supersedeReport(old.Acceptance.RelationshipID, "I", "rp", "second")
		replace := AcceptInput{PullRequest: acceptTargetPull.PullRequest, Supersedes: old.Acceptance.AcceptanceID}
		_, err := k.accept("rp", "I", replace)
		if reason, detail := acceptTargetRefusal(err); reason != "disposition_conflict" || detail != fmt.Sprintf(acceptTargetRemedy, acceptTargetCheckout) {
			t.Fatalf("replacement = %s: %s", reason, detail)
		}
		if _, state := stored(k, old.Acceptance.AcceptanceID); state != "active" || k.acceptCount() != 1 || k.acceptTargetForgeRows() != 1 ||
			k.count("SELECT COUNT(*) FROM dag_acceptances WHERE state <> 'active' OR supersedes_acceptance_id IS NOT NULL") != 0 {
			t.Fatalf("the refusal left acceptance %s %q, %d acceptances, %d forge rows", old.Acceptance.AcceptanceID, state, k.acceptCount(), k.acceptTargetForgeRows())
		}
		// the coordinator moves the edge to the forge: the same call is accepted and supersedes the legacy acceptance
		k.putPlan("rp", int(k.snapshot("rp").Revision), "rp-r2", doc{"op": dag.OpRetireEdge, "edge_id": "ix"}, addEdge("ix-forge", "I", "X", dag.EdgeIntegrated, doc{"target_repository": "owner/repo"}))
		res, err := k.accept("rp", "I", replace)
		if err != nil || res.SupersededID != old.Acceptance.AcceptanceID || k.acceptCount() != 2 || k.acceptTargetForgeRows() != 2 {
			t.Fatalf("replacement on the forge = %v %+v (%d acceptances, %d forge rows)", err, res, k.acceptCount(), k.acceptTargetForgeRows())
		}
		if repository, _ := stored(k, res.AcceptanceID); repository != "owner/repo" {
			t.Fatalf("the new acceptance's target = %q", repository)
		}
		if _, state := stored(k, old.Acceptance.AcceptanceID); state != "superseded" {
			t.Fatalf("the legacy acceptance is %q, want superseded", state)
		}
	})
}
