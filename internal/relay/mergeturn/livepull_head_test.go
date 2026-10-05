package mergeturn

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

// CRW-538: a head that is declared on a turn bound to a pull request is the head of that pull request.
// merge-turn-ready and merge-turn-check read the pull request head from the forge, because nothing the
// turn records says which head belongs to which pull request.

// livePullHeads is a fake forge: the head of each pull request, the reads it answered, and the ones that fail.
type livePullHeads struct {
	mu    sync.Mutex
	heads map[string]string
	fail  map[string]string
	reads int
}

func newLivePullHeads() *livePullHeads {
	return &livePullHeads{heads: map[string]string{}, fail: map[string]string{}}
}

func livePullKey(repository string, number int64) string {
	return fmt.Sprintf("%s#%d", repository, number)
}

func (f *livePullHeads) set(repository string, number int64, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.heads[livePullKey(repository, number)] = sha
}

func (f *livePullHeads) breakRead(repository string, number int64, why string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fail[livePullKey(repository, number)] = why
}

func (f *livePullHeads) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func (f *livePullHeads) PullRequestHead(_ context.Context, repository string, number int64) (PullRequestHeadReading, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if why, broken := f.fail[livePullKey(repository, number)]; broken {
		return PullRequestHeadReading{}, &TargetUnreadable{why}
	}
	sha, ok := f.heads[livePullKey(repository, number)]
	if !ok {
		return PullRequestHeadReading{}, &TargetUnreadable{"the test set no head for " + livePullKey(repository, number)}
	}
	return PullRequestHeadReading{Repository: repository, Number: number, SHA: sha, Source: "fake forge"}, nil
}

// livePullReader is the Reader merge-turn-check receives in production: a base tip and a pull request head.
type livePullReader struct {
	target *fakeTarget
	pulls  *livePullHeads
}

func (r livePullReader) Tip(ctx context.Context, repository, base string) (Tip, error) {
	return r.target.Tip(ctx, repository, base)
}

func (r livePullReader) PullRequestHead(ctx context.Context, repository string, number int64) (PullRequestHeadReading, error) {
	return r.pulls.PullRequestHead(ctx, repository, number)
}

// livePullBound is a holding turn bound to pull request 500 at head-500, its grant answered, with the fake forge wired in.
func livePullBound(t *testing.T) (*fx, *livePullHeads, string) {
	t.Helper()
	w := newFx(t)
	pulls := newLivePullHeads()
	pulls.set(fxRepo, 500, "head-500")
	w.m.Pulls = pulls
	turn := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, "rel-500"))["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	return w, pulls, turn
}

func livePullDecision(t *testing.T, answer map[string]any, head string) {
	t.Helper()
	decided, _ := answer["pullRequestHead"].(map[string]any)
	if decided == nil || decided["decidedBy"] != "forge" || decided["head"] != head || decided["pullRequest"] != int64(500) || decided["source"] != "fake forge" {
		t.Fatalf("the answer does not say the forge decided the head: %v", answer["pullRequestHead"])
	}
}

// The 2026-10-04 case: pull request 501's head declared on the turn of pull request 500.
func TestLivePullReadyRefusesAnotherPullRequestsHead(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	pulls.set(fxRepo, 501, "head-501")
	ledger := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")

	answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-501", "refreshed the base of pull request 501")
	if reasonOf(err) != "merge_candidate_moved" || answer != nil {
		t.Fatalf("the head of another pull request was accepted: %v %v", answer, err)
	}
	detail := livePullDetail(err)
	for _, want := range []string{turn, "pull request 500", "head-500", "head-501", "read the pull request again"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal does not say %q: %s", want, detail)
		}
	}
	live := w.must(w.m.Turn(w.ctx, turn))
	if live["candidateHead"] != "head-500" || live["state"] != Holding {
		t.Errorf("the turn took the head: %v", live)
	}
	if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger"); got != ledger {
		t.Errorf("a refused declaration wrote ledger rows: %d, was %d", got, ledger)
	}
	if pulls.readCount() != 1 {
		t.Errorf("%d forge reads", pulls.readCount())
	}
	key, _ := TargetKey(fxRepo, fxBase)
	contests, err := w.r.CoordinationConflicts(w.ctx, registry.DomainMergeTarget, key)
	if err != nil || len(contests) != 1 || contests[0].Get("reason") != "merge_candidate_moved" || contests[0].Get("incumbent") != "head-500" || contests[0].Get("challenger") != "head-501" {
		t.Errorf("the refusal was not kept as a contest of the target: %v %v", contests, err)
	}
}

// The ordinary flow: the holder refreshes its own pull request branch inside the turn and restates the head it shows.
func TestLivePullReadyAcceptsTheRefreshedHeadOfItsOwnPullRequest(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	pulls.set(fxRepo, 500, "head-500-refreshed")
	answer := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-500-refreshed", "refreshed the base"))
	if answer["candidateHead"] != "head-500-refreshed" || answer["declaredReady"] != false {
		t.Fatalf("the refreshed head was not taken: %v", answer)
	}
	livePullDecision(t, answer, "head-500-refreshed")
	reset, _ := answer["readinessReset"].(map[string]any)
	if reset == nil || reset["previousHead"] != "head-500" || reset["grantId"] == nil {
		t.Fatalf("the restated head lost its reset or its grant: %v", answer["readinessReset"])
	}
}

// A forge that cannot answer decides nothing: the declaration is refused and nothing is written.
func TestLivePullReadyFailsClosedWhenThePullRequestIsNotRead(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	pulls.breakRead(fxRepo, 500, "the forge timed out")
	ledger := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger")
	_, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, "head-500-refreshed", "")
	if reasonOf(err) != "merge_target_unreadable" || !strings.Contains(livePullDetail(err), "the forge timed out") || !strings.Contains(livePullDetail(err), "pull request 500") {
		t.Fatalf("an unread pull request was not refused as merge_target_unreadable: %v", err)
	}
	if live := w.must(w.m.Turn(w.ctx, turn)); live["candidateHead"] != "head-500" {
		t.Errorf("the turn took the head: %v", live["candidateHead"])
	}
	if got := livePullCount(w, "SELECT COUNT(*) FROM merge_turn_ledger"); got != ledger {
		t.Errorf("a refused declaration wrote ledger rows: %d, was %d", got, ledger)
	}
}

// Heads are the same commit however their text is cased or padded, as the base tip is compared.
func TestLivePullReadyComparesHeadsAsCommits(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	pulls.set(fxRepo, 500, "head-500-refreshed")
	answer := w.must(w.m.Ready(w.ctx, turn, alpha.TaskID, false, " HEAD-500-REFRESHED ", "refreshed the base"))
	livePullDecision(t, answer, "head-500-refreshed")
}

// The head that was read is the one that decides. A head that moved between the early read of the turn and its
// transaction (a concurrent call) was not read, so it is not accepted on the strength of a read made for another.
func TestLivePullReadyRefusesAHeadThatMovedAfterTheEarlyRead(t *testing.T) {
	w, _, turn := livePullBound(t)
	moved := false
	w.m.Now = func() string {
		if !moved {
			moved = true
			w.exec("UPDATE merge_turns SET candidate_head = ? WHERE turn_id = ?", "head-elsewhere", turn)
		}
		return fxISO
	}
	// head-500 is the candidate when the call begins, so nothing is read for it; by the transaction it is a moved head
	_, err := w.m.Ready(w.ctx, turn, alpha.TaskID, true, "head-500", "")
	if reasonOf(err) != "merge_target_unreadable" || !strings.Contains(livePullDetail(err), "changed during the call") {
		t.Fatalf("a head that moved without being read was accepted: %v", err)
	}
	if live := w.must(w.m.Turn(w.ctx, turn)); live["candidateHead"] != "head-elsewhere" {
		t.Errorf("the refused call changed the turn: %v", live["candidateHead"])
	}
}

// Where nothing is compared nothing is read: a head that did not move without readiness being declared on it (a withdrawal
// asserts nothing about the head), and a turn that records no pull request and no relationship. Declaring readiness on a
// head that did not move is compared too (headcompare_test.go).
func TestLivePullReadyReadsNothingWhenNothingIsCompared(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	for _, head := range []string{"head-500", ""} {
		answer, err := w.m.Ready(w.ctx, turn, alpha.TaskID, false, head, "")
		if err != nil || answer["pullRequestHead"] != nil {
			t.Fatalf("an unchanged head: %v %v", answer, err)
		}
	}
	if pulls.readCount() != 0 {
		t.Fatalf("a head that did not move read the forge %d times", pulls.readCount())
	}

	w2 := newFx(t)
	pulls2 := newLivePullHeads()
	w2.m.Pulls = pulls2
	unbound := w2.must(livePullClaim(w2, alpha, fxA, "head-a", 0, ""))["turnId"].(string)
	answer := w2.must(w2.m.Ready(w2.ctx, unbound, alpha.TaskID, false, "head-anything", ""))
	if answer["candidateHead"] != "head-anything" || answer["pullRequestHead"] != nil || pulls2.readCount() != 0 {
		t.Fatalf("a turn bound to no pull request: %v, %d reads", answer, pulls2.readCount())
	}
}

// A pull request on a repository that is a local path has no forge to read from the turn alone. The old answer was that
// the record decided; now the work report of the turn's relationship decides, or the call is refused: see
// TestHeadCompareLocalPathPullRequestReadyNeedsAWorkReport and the tests after it.

// A claim that waits is bound to its pull request as well.
func TestLivePullReadyChecksAWaitingClaimToo(t *testing.T) {
	w := newFx(t)
	pulls := newLivePullHeads()
	pulls.set(fxRepo, 500, "head-500")
	w.m.Pulls = pulls
	w.must(livePullClaim(w, beta, fxB, "head-b", 9, ""))
	waiting := w.must(livePullClaim(w, alpha, fxA, "head-500", 500, ""))["turnId"].(string)
	if _, err := w.m.Ready(w.ctx, waiting, alpha.TaskID, false, "head-501", ""); reasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("a waiting claim took another pull request's head: %v", err)
	}
	pulls.set(fxRepo, 500, "head-500-refreshed")
	if got := w.must(w.m.Ready(w.ctx, waiting, alpha.TaskID, false, "head-500-refreshed", "")); got["candidateHead"] != "head-500-refreshed" {
		t.Fatalf("a waiting claim refused its own pull request's head: %v", got)
	}
}

func livePullCheck(w *fx, turn, head string, reader Reader) (map[string]any, error) {
	b := defaults()
	b.head, b.checks = head, runChecks(head, "success", 1, "dev-gate", "run-1")
	return w.m.Check(w.ctx, turn, b.actor, b.head, b.base, b.checks, b.review, b.required, reader)
}

// A turn an older relay let declare another pull request's head is refused where the merge begins.
func TestLivePullCheckRefusesAHeadThatIsNotThePullRequests(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	w.exec("UPDATE merge_turns SET candidate_head = ? WHERE turn_id = ?", "head-501", turn)
	reader := livePullReader{target: w.target, pulls: pulls}

	_, err := livePullCheck(w, turn, "head-501", reader)
	if reasonOf(err) != "merge_candidate_moved" {
		t.Fatalf("the check went on with another pull request's head: %v", err)
	}
	detail := livePullDetail(err)
	for _, want := range []string{"pull request 500", "head-500", "head-501"} {
		if !strings.Contains(detail, want) {
			t.Errorf("the refusal does not say %q: %s", want, detail)
		}
	}
	if live := w.must(w.m.Turn(w.ctx, turn)); live["state"] != Holding {
		t.Fatalf("a refused check began the merge: %v", live["state"])
	}

	w.exec("UPDATE merge_turns SET candidate_head = ? WHERE turn_id = ?", "head-500", turn)
	answer, err := livePullCheck(w, turn, "head-500", reader)
	if err != nil {
		t.Fatalf("the pull request's own head was refused: %v", err)
	}
	if answer["state"] != Merging {
		t.Fatalf("the check did not begin the merge: %v", answer["state"])
	}
	livePullDecision(t, answer, "head-500")
}

// The check compares the pull request's head with the restated one as commits too: the forge's text is not the declared text.
func TestLivePullCheckComparesHeadsAsCommits(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	pulls.set(fxRepo, 500, " HEAD-500 ")
	answer, err := livePullCheck(w, turn, "head-500", livePullReader{target: w.target, pulls: pulls})
	if err != nil || answer["state"] != Merging {
		t.Fatalf("a head that is the same commit was refused: %v %v", answer, err)
	}
}

func TestLivePullCheckFailsClosedWhenThePullRequestIsNotRead(t *testing.T) {
	w, pulls, turn := livePullBound(t)
	pulls.breakRead(fxRepo, 500, "the forge timed out")
	_, err := livePullCheck(w, turn, "head-500", livePullReader{target: w.target, pulls: pulls})
	if reasonOf(err) != "merge_target_unreadable" || !strings.Contains(livePullDetail(err), "pull request 500") {
		t.Fatalf("an unread pull request was not refused as merge_target_unreadable: %v", err)
	}
	if live := w.must(w.m.Turn(w.ctx, turn)); live["state"] != Holding {
		t.Fatalf("a refused check began the merge: %v", live["state"])
	}
}

// A reader that cannot read pull requests (every fake before this change, a relay with no forge reader) leaves the check as it was.
func TestLivePullCheckWithoutAPullRequestReaderIsUnchanged(t *testing.T) {
	w, _, turn := livePullBound(t)
	answer, err := w.check(turn, "head-500", "base-0", "")
	if err != nil || answer["state"] != Merging || answer["pullRequestHead"] != nil {
		t.Fatalf("%v %v", answer, err)
	}
}
