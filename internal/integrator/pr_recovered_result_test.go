package integrator

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// CRW-1413 item 1: a head already in dev with no landing record is recorded once after the fact; that recovery is what the pass
// reports: its result is landed-before-record, its status 0 (the head is in the branch and nothing is wrong), its Landing the
// recovered record, no Summary (no run ran). It ends the pass: the pull requests and issue heads behind it wait for the next pass.

// secondMilestone publishes the branch crw/m2 with one file and opens its pull request (number 2) toward dev.
func (w *prWorld) secondMilestone(issue string) tracking.PullRequest {
	w.t.Helper()
	w.head("crw/m2", map[string]string{"issue-" + issue + ".txt": issue + "\n"}, true)
	ref, err := tracking.ParseRef("o/r#" + issue)
	if err != nil {
		w.t.Fatal(err)
	}
	pr, err := w.gh.OpenPullRequest(context.Background(), tracking.NewPullRequest{Head: "crw/m2", Base: "dev", Title: "milestone M2", Issues: []tracking.Ref{ref}})
	if err != nil {
		w.t.Fatal(err)
	}
	return pr
}

// unreadableOne is a pull request source that cannot read pull request number 1, as GitHub failing for that one request.
type unreadableOne struct{ PullRequestSource }

func (u unreadableOne) PullRequest(ctx context.Context, ref tracking.Ref) (tracking.PullRequest, error) {
	if ref.Number == 1 {
		return tracking.PullRequest{}, errors.New("boom")
	}
	return u.PullRequestSource.PullRequest(ctx, ref)
}

// removeLandingAndPend is crashAfterPush for the entry name: the landing record and the done/ file of the first run are removed and
// the head is pending again.
func (w *prWorld) removeLandingAndPend(name string, rep Report, head string) {
	w.t.Helper()
	if err := os.Remove(filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json")); err != nil {
		w.t.Fatal(err)
	}
	moved, _ := filepath.Glob(filepath.Join(w.cfg.QueueDir, "done", name+"*"))
	for _, p := range moved {
		if err := os.Remove(p); err != nil {
			w.t.Fatal(err)
		}
	}
	w.pend(name, head)
}

// TestARecoveredLandingIsThePassResult: one pending pull request whose head is in dev with no record.
func TestARecoveredLandingIsThePassResult(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)

	rep := w.pass()
	requireExit(t, rep, ExitPushed, resultReportLandedBeforeRecord)
	if rep.Summary != nil {
		t.Errorf("a recovered landing ran nothing, yet the report has a summary %+v", rep.Summary)
	}
	if rep.Landing == nil || rep.Landing.Result != "landed_before_record" || rep.Landing.Number != 1 || rep.Landing.Head != pr.HeadSHA {
		t.Errorf("the pass reports %+v", rep.Landing)
	}
	if rep.Error != "" {
		t.Errorf("error %q", rep.Error)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("pr-1 is not in done/ (moved %q)", rep.Moved)
	}
	if w.remoteDev() != dev || w.ctlRead("verify-n") != "1\n" {
		t.Errorf("the recovery ran again: dev %s, verified %q", w.remoteDev(), w.ctlRead("verify-n"))
	}
}

// TestARecoveredLandingIsNotOverwrittenByAnotherPullRequest: a second pull request is pending too. The report carries the first
// one's landing, the second stays pending and unverified that pass, and the next pass lands it.
func TestARecoveredLandingIsNotOverwrittenByAnotherPullRequest(t *testing.T) {
	w := newPRWorld(t, true)
	pr1 := w.milestone("10")
	w.secondMilestone("2")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr1.HeadSHA)
	pr2 := w.enqueue("o/r#2")

	rep := w.pass()
	requireExit(t, rep, ExitPushed, resultReportLandedBeforeRecord)
	if rep.Landing == nil || rep.Landing.Number != 1 || rep.Landing.Result != "landed_before_record" {
		t.Fatalf("the pass reports %+v", rep.Landing)
	}
	if w.pendingHead("pr-2") != pr2.Head || w.remoteDev() != dev || w.ctlRead("verify-n") != "1\n" {
		t.Errorf("pr-2 was touched in the pass of the recovery: pending %q, dev %s, verified %q", w.pendingHead("pr-2"), w.remoteDev(), w.ctlRead("verify-n"))
	}
	if n := len(w.fake.Comments(2)); n != 0 {
		t.Errorf("pr 2 got %d comments in the recovery pass", n)
	}

	next := w.pass()
	requireExit(t, next, ExitPushed, "pushed")
	if next.Landing == nil || next.Landing.Number != 2 || next.Landing.Result != "pushed" || !w.doneFor("pr-2", pr2.Head) {
		t.Errorf("the next pass %+v (moved %q)", next.Landing, next.Moved)
	}
}

// TestARecoveredLandingEndsThePassBeforeTheIssueHeads: an issue head is pending beside the recovered pull request; it is untouched
// that pass and lands in the next, whose report has no landing.
func TestARecoveredLandingEndsThePassBeforeTheIssueHeads(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	h3 := w.head("h3", map[string]string{"f3.txt": "3\n"}, true)
	w.pend("3", h3)

	rep := w.pass()
	requireExit(t, rep, ExitPushed, resultReportLandedBeforeRecord)
	if rep.Summary != nil || rep.Landing == nil || rep.Landing.Number != 1 {
		t.Errorf("pass %+v", rep)
	}
	w.mustWhere("3", "pending")
	if w.remoteDev() != dev || w.ctlRead("verify-n") != "1\n" {
		t.Errorf("the issue head was integrated in the recovery pass: dev %s, verified %q", w.remoteDev(), w.ctlRead("verify-n"))
	}

	next := w.pass()
	requireExit(t, next, ExitPushed, "pushed")
	if next.Landing != nil || next.Summary == nil || next.Summary.Merged != "crw-3="+h3 {
		t.Errorf("the next pass %+v", next)
	}
	w.mustWhere("3", "done")
}

// TestARecoveredLandingWithAnUnreadablePullRequestAheadIsStillThePassResult: pull request 1 cannot be read and is held; pull request 2
// is recovered. The pass result is the recovery with status 0, and Held lists pull request 1.
func TestARecoveredLandingWithAnUnreadablePullRequestAheadIsStillThePassResult(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	pr2 := w.secondMilestone("2")
	w.enqueue("o/r#2")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	w.removeLandingAndPend("pr-2", first, pr2.HeadSHA)
	w.enqueue("o/r#1")
	w.r.PullRequests = unreadableOne{w.r.PullRequests}

	rep := w.pass()
	requireExit(t, rep, ExitPushed, resultReportLandedBeforeRecord)
	if rep.Landing == nil || rep.Landing.Number != 2 || rep.Landing.Result != "landed_before_record" {
		t.Fatalf("the pass reports %+v", rep.Landing)
	}
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: the pull request could not be read") {
		t.Errorf("held %q", rep.Held)
	}
	w.mustWhere("pr-1", "pending")
	if !w.doneFor("pr-2", pr2.HeadSHA) {
		t.Errorf("pr-2 is not in done/ (moved %q)", rep.Moved)
	}
}
