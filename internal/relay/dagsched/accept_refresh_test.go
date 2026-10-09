package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-731. dag-accept proves, before it writes, that the head a pull request shows is the head the verdict fixed
// (dag_verified_heads, CRW-742) plus merges of the base and nothing else, and stores the proof beside the acceptance
// (dag_acceptance_refreshes, CRW-728). The ruled head is read from the ruling's record; the call carries no head. Every
// commit is a real commit of a real repository, the pull request is read from the scripted forge, and the acceptance
// goes through Scheduler.Accept.

// acceptRefreshKit is node I of the integration kit, reported and ruled verified at the head p of its branch.
type acceptRefreshKit struct {
	*integrationKit
	rid, event, criteria string
	p                    string
}

func newAcceptRefreshKit(t *testing.T) *acceptRefreshKit { return newAcceptRefreshKitWith(t, true) }

// newAcceptRefreshKitWith is the kit with the ruling's record of the head (ruled) or without it, as an event ruled before CRW-742 is.
func newAcceptRefreshKitWith(t *testing.T, ruled bool) *acceptRefreshKit {
	t.Helper()
	k := newIntegrationKit(t)
	repo := k.repo
	repo.git("checkout", "-q", "-b", "feature")
	p := repo.commit("feature.txt", "feature\n")
	repo.git("checkout", "-q", "dev")
	k.declare("g", "I", "feature.txt")
	r := k.reportNode("g", "I", acceptOpts{})
	n, _ := nodeOf(k.snapshot("g"), "I")
	a := &acceptRefreshKit{integrationKit: k, rid: r.Acceptance.RelationshipID, event: r.Event, criteria: n.CriteriaSetDigest, p: p}
	if ruled {
		a.rule(a.event, 1, p)
	}
	k.forge.by["owner/repo#7"] = openPR("owner/repo", 7, p)
	return a
}

// rule records the head a verdict turn verified for an event, as verdict --verified-head does.
func (a *acceptRefreshKit) rule(event string, generation int64, head string) {
	a.t.Helper()
	a.exec("INSERT INTO dag_verified_heads (event_id, relationship_id, execution_generation, verdict_turn_id, head_sha, recorded_by_task_id, recorded_at) VALUES (?,?,?,?,?,?,?)",
		event, a.rid, generation, "verdict-turn", head, "parent", a.clock())
}

// pointAt shows a head on the pull request.
func (a *acceptRefreshKit) pointAt(head string) {
	a.t.Helper()
	a.forge.by["owner/repo#7"] = openPR("owner/repo", 7, head)
}

// refresh is the parent's clean refresh: a commit on dev and one merge of dev into the branch of the pull request. It answers the new head and shows it on the pull request.
func (a *acceptRefreshKit) refresh(file string) string {
	a.t.Helper()
	repo := a.repo
	repo.commit(file, "dev\n")
	repo.git("checkout", "-q", "feature")
	repo.git("merge", "-q", "--no-ff", "-m", "merge dev "+file, "dev")
	head := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "-q", "dev")
	a.pointAt(head)
	return head
}

// resolveByHand is a refresh that conflicts on feature.txt and resolves it by hand.
func (a *acceptRefreshKit) resolveByHand() string {
	a.t.Helper()
	repo := a.repo
	repo.commit("feature.txt", "dev changed the feature\n")
	repo.git("checkout", "-q", "feature")
	if _, err := repo.tryGit("merge", "-q", "--no-ff", "-m", "merge dev", "dev"); err == nil {
		a.t.Fatal("the merge did not conflict")
	}
	repo.write("feature.txt", "hand resolved: something neither side wrote\n")
	repo.git("add", "feature.txt")
	repo.git("commit", "-q", "-m", "merge dev, feature.txt resolved")
	head := repo.git("rev-parse", "HEAD")
	repo.git("checkout", "-q", "dev")
	a.pointAt(head)
	return head
}

func (a *acceptRefreshKit) acceptWith(checkout string) (AcceptResult, error) {
	a.t.Helper()
	return a.accept("g", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}, Checkout: checkout})
}

func (a *acceptRefreshKit) refreshRows() int {
	return a.count("SELECT COUNT(*) FROM dag_acceptance_refreshes")
}

// A head the ruling did not fix, with no checkout to prove it in, is malformed_receipt; a checkout that lacks one of the three
// commits is merge_target_unreadable. Neither writes anything.
func TestAcceptProvesARefreshedHeadInTheCheckout(t *testing.T) {
	a := newAcceptRefreshKit(t)
	a.refresh("other.txt")
	if _, err := a.acceptWith(""); refusalReason(err) != "malformed_receipt" || !strings.Contains(err.Error(), "--checkout") {
		t.Fatalf("accept without a checkout = %v, want malformed_receipt naming the checkout", err)
	}
	empty := newGitRepo(t)
	if _, err := a.acceptWith(empty.path); refusalReason(err) != "merge_target_unreadable" || !strings.Contains(err.Error(), "fetch") {
		t.Fatalf("accept in a checkout without the commits = %v, want merge_target_unreadable naming the fetch", err)
	}
	if a.acceptCount() != 0 || a.refreshRows() != 0 {
		t.Fatalf("%d acceptances and %d refresh rows written by refused calls", a.acceptCount(), a.refreshRows())
	}
}

// A clean refresh is accepted and its proof row is written with it, naming the ruled head, the accepted head and the base tip; two
// refreshes in a row are one proof.
func TestAcceptRecordsTheProofOfARefreshChain(t *testing.T) {
	a := newAcceptRefreshKit(t)
	a.refresh("other-one.txt")
	head := a.refresh("other-two.txt")
	res, err := a.acceptWith(a.repo.path)
	if err != nil || res.HeadSHA != head || res.VerifiedHead != a.p || res.RefreshID == "" || res.Manual == nil || len(res.Manual) != 0 {
		t.Fatalf("accept = %v %+v, want the refreshed head accepted with the ruled head %s and a proof", err, res, a.p)
	}
	if a.refreshRows() != 1 {
		t.Fatalf("%d refresh rows, want the chain as one proof", a.refreshRows())
	}
	row, err := store.AcceptanceRefresh(context.Background(), a.s, res.AcceptanceID, 1)
	if err != nil || row.RefreshID != res.RefreshID || row.VerifiedHeadSHA != a.p || row.HeadSHA != head || row.BaseTipSHA != a.repo.git("rev-parse", "dev") ||
		row.BaseRepository != "owner/repo" || row.BaseRef != "dev" || row.EventID != a.event || row.RelationshipID != a.rid || row.ExecutionGeneration != 1 {
		t.Fatalf("proof row = %+v (%v)", row, err)
	}
	if got := strings.Count(row.ProofJSON, `"base_parent"`); got != 2 {
		t.Fatalf("the proof holds %d merges, want both: %s", got, row.ProofJSON)
	}
	if row.ResolvedPathsJSON != "[]" {
		t.Fatalf("resolved paths %s, want none", row.ResolvedPathsJSON)
	}
	// the same output again is a replay and needs no checkout and writes no second proof
	again, err := a.acceptWith("")
	if err != nil || !again.Replayed || again.AcceptanceID != res.AcceptanceID || again.RefreshID != "" || a.refreshRows() != 1 {
		t.Fatalf("replay = %v %+v (rows %d)", err, again, a.refreshRows())
	}
}

// The reproduction of the post-merge finding P1-1 of the CRW-666 pull request: the verdict fixed p, then the parent resolved a code
// conflict by hand and pushed that head. No flag names a head, so the call cannot call the hand-resolved head the ruled one; the
// relay refuses it as tree_differs and writes nothing.
func TestAcceptRefusesAHandResolvedHeadTheRulingDidNotFix(t *testing.T) {
	a := newAcceptRefreshKit(t)
	a.resolveByHand()
	_, err := a.acceptWith(a.repo.path)
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), RefreshTreeDiffers) {
		t.Fatalf("accept of a hand-resolved head = %v, want disposition_conflict with tree_differs", err)
	}
	if a.acceptCount() != 0 || a.refreshRows() != 0 {
		t.Fatalf("%d acceptances and %d refresh rows written for a head no rule proved", a.acceptCount(), a.refreshRows())
	}
}

// A head that is not the ruled head plus merges of the base (a commit of its own on top) is refused with the proof's own code.
func TestAcceptRefusesAHeadWithCommitsBeyondTheMerges(t *testing.T) {
	a := newAcceptRefreshKit(t)
	a.repo.git("checkout", "-q", "feature")
	head := a.repo.commit("extra.txt", "a change after the ruling\n")
	a.repo.git("checkout", "-q", "dev")
	a.pointAt(head)
	_, err := a.acceptWith(a.repo.path)
	if refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), "not a base refresh") {
		t.Fatalf("accept = %v, want the proof's refusal", err)
	}
	if a.acceptCount() != 0 || a.refreshRows() != 0 {
		t.Fatal("a refused proof wrote rows")
	}
}

// With no record of the ruled head the call is what it was: the head the forge shows is accepted, no checkout is read, no proof row
// is written and the new members of the answer are empty.
func TestAcceptWithoutARulingRecordIsAsItWas(t *testing.T) {
	a := newAcceptRefreshKitWith(t, false)
	head := a.refresh("other.txt")
	res, err := a.acceptWith("")
	if err != nil || res.HeadSHA != head || res.VerifiedHead != "" || res.RefreshID != "" || res.Manual != nil || a.refreshRows() != 0 {
		t.Fatalf("accept = %v %+v (rows %d), want the head shown accepted with the new members empty", err, res, a.refreshRows())
	}
}

// P == N needs no checkout and writes no proof row; the ruled head is reported.
func TestAcceptAtTheRuledHeadNeedsNoCheckout(t *testing.T) {
	a := newAcceptRefreshKit(t)
	res, err := a.acceptWith("")
	if err != nil || res.HeadSHA != a.p || res.VerifiedHead != a.p || res.RefreshID != "" || res.Manual != nil || a.refreshRows() != 0 {
		t.Fatalf("accept = %v %+v (rows %d), want the ruled head accepted without a proof", err, res, a.refreshRows())
	}
}

// correct opens the correction generation 2 of the accepted node by hand, as dag-correct records it, and answers the event of the
// child's report there, ruled verified at head p2 (the branch moves on to it).
func (a *acceptRefreshKit) correct(p2 string) string {
	a.t.Helper()
	ctx := context.Background()
	roots, err := relationshipRoots(ctx, a.s.Q(ctx), a.rid)
	if err != nil || len(roots) == 0 {
		a.t.Fatalf("the artifact roots of the child: %v %v", roots, err)
	}
	notes := writeFile(a.t, roots[0], "rework-notes.md", "the notes of the rework")
	prepared, err := a.sched.PrepareCorrection(ctx, "g", "I", "parent", ManifestInput{Base: &BaseRef{Repository: "owner/repo", Ref: "dev"}, RuleVersion: a.request(false).RuleVersion,
		Volatile: []Volatile{{Source: "linear:comment", SnapshotURI: notes, SHA256: shaOf([]byte("the notes of the rework")), CapturedAt: "2026-10-02T00:00:00Z"}}}, VerifyOptions{ArtifactRoots: roots})
	if err != nil {
		a.t.Fatalf("prepare: %v", err)
	}
	acOpenByHand(a.t, a.releaseKit, a.rid, prepared.DispatchRequestID, "needs_changes_revision", 2, true)
	if _, err := a.sched.RecordCorrection(context.Background(), "g", "I", "parent", prepared.ManifestDigest); err != nil {
		a.t.Fatalf("dag-correct: %v", err)
	}
	event := a.rvReportGeneration(a.rid, "I", 2, a.criteria)
	a.rule(event, 2, p2)
	return event
}

// A refused proof leaves the acceptance it would have replaced active, and a proved one replaces it and stores its proof.
func TestAcceptSupersedeKeepsTheOldAcceptanceWhenTheProofIsRefused(t *testing.T) {
	a := newAcceptRefreshKit(t)
	first, err := a.acceptWith("")
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	// the correction: the child's branch moves to p2, the ruling fixes it, then the parent resolves a conflict by hand
	a.repo.git("checkout", "-q", "feature")
	p2 := a.repo.commit("feature.txt", "feature corrected\n")
	a.repo.git("checkout", "-q", "dev")
	a.correct(p2)
	a.resolveByHand()
	in := AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}, Checkout: a.repo.path, Supersedes: first.AcceptanceID}
	if _, err := a.accept("g", "I", in); refusalReason(err) != "disposition_conflict" || !strings.Contains(err.Error(), RefreshTreeDiffers) {
		t.Fatalf("supersede with a hand-resolved head = %v, want tree_differs", err)
	}
	var active string
	if err := a.s.DB.QueryRow("SELECT acceptance_id FROM dag_acceptances WHERE plan_id = 'g' AND node_id = 'I' AND state = 'active'").Scan(&active); err != nil || active != first.AcceptanceID {
		t.Fatalf("active acceptance = %q (%v), want the first one %q", active, err, first.AcceptanceID)
	}
	if a.acceptCount() != 1 || a.refreshRows() != 0 {
		t.Fatalf("%d acceptances and %d refresh rows, want the first only", a.acceptCount(), a.refreshRows())
	}
}

func TestAcceptSupersedeStoresTheProofOfTheCorrectedHead(t *testing.T) {
	a := newAcceptRefreshKit(t)
	first, err := a.acceptWith("")
	if err != nil {
		t.Fatalf("first accept: %v", err)
	}
	a.repo.git("checkout", "-q", "feature")
	p2 := a.repo.commit("feature.txt", "feature corrected\n")
	a.repo.git("checkout", "-q", "dev")
	event := a.correct(p2)
	head := a.refresh("other.txt")
	res, err := a.accept("g", "I", AcceptInput{PullRequest: &PRRef{Repository: "owner/repo", Number: 7}, Checkout: a.repo.path, Supersedes: first.AcceptanceID})
	if err != nil || res.SupersededID != first.AcceptanceID || res.HeadSHA != head || res.VerifiedHead != p2 || res.RefreshID == "" {
		t.Fatalf("supersede = %v %+v", err, res)
	}
	row, err := store.AcceptanceRefresh(context.Background(), a.s, res.AcceptanceID, 1)
	if err != nil || row.VerifiedHeadSHA != p2 || row.HeadSHA != head || row.EventID != event || row.ExecutionGeneration != 2 {
		t.Fatalf("proof row = %+v (%v), want the proof from the corrected head p2 on generation 2", row, err)
	}
}

// The answer's manual_paths member is null when no refresh was proved and a list (empty: a path no rule proved refuses the acceptance) when one was.
func TestAcceptManualPathsAreNullWithoutAProof(t *testing.T) {
	if got := acceptManualPaths(nil); got != nil {
		t.Fatalf("manual_paths without a proof = %#v, want null", got)
	}
	list, ok := acceptManualPaths([]string{}).([]any)
	if !ok || len(list) != 0 {
		t.Fatalf("manual_paths with a proof = %#v, want an empty list", acceptManualPaths([]string{}))
	}
}
