package integrator

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// CRW-1355 fix round 1: a pull request entry is keyed by its number and its head. The head that lands is the head GitHub still
// names right before dev moves; a done/ entry stands for one head only, so new commits on the branch land through a new enqueue;
// and the landing record of one (number, head) is written once, also when a pass ended after the push and before the record.

// pushToMilestone commits one more file on crw/m1 in the other clone and publishes it, as a project session that lands one more
// node on its milestone branch; it answers the new head.
func (w *prWorld) pushToMilestone(file string) string {
	w.t.Helper()
	w.git(w.other, "checkout", "-q", "crw/m1")
	w.write(filepath.Join(w.other, file), file+"\n")
	w.git(w.other, "add", "-A")
	w.git(w.other, "commit", "-q", "-m", file)
	w.git(w.other, "push", "-q", "origin", "crw/m1")
	return w.git(w.other, "rev-parse", "HEAD")
}

// landings is every landing record in runs_dir.
func (w *prWorld) landings() []Landing {
	w.t.Helper()
	paths, _ := filepath.Glob(filepath.Join(w.cfg.RunsDir, "*.pr.json"))
	var out []Landing
	for _, p := range paths {
		out = append(out, readLanding(w.t, p))
	}
	return out
}

func (w *prWorld) doneFor(name, head string) bool {
	_, err := os.Stat(filepath.Join(w.cfg.QueueDir, "done", name+"-"+head))
	return err == nil
}

func (w *prWorld) pendingHead(name string) string {
	h, _ := readHead(filepath.Join(w.cfg.QueueDir, "pending", name))
	return h
}

// TestPushDuringVerifyKeepsThePullRequestPendingAndLandsTheNewHead is the reviewer's
// TestPushDuringVerifyLandsStaleHeadAndDoneSkipsNewCommits turned into the expected behaviour: a push to the milestone branch while
// the queue verifies moves no dev for the old head; the entry stays pending with the new head and the record says head_moved; the
// next pass verifies, reviews and lands the new head.
func TestPushDuringVerifyKeepsThePullRequestPendingAndLandsTheNewHead(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	w.ctlWrite("during-verify.sh", `[ -f "$CTL_DIR/pushed-once" ] && exit 0
touch "$CTL_DIR/pushed-once"
cd "$OTHER_DIR" && git checkout -q crw/m1 && echo later > later.txt && git add -A && git commit -q -m later && git push -q origin crw/m1
`)
	base := w.remoteDev()
	first := w.pass()
	newHead := w.git(w.remote, "rev-parse", "crw/m1")
	if newHead == pr.HeadSHA {
		t.Fatal("the push during the verification did not happen")
	}
	requireExit(t, first, ExitMoved, "head-moved:"+newHead)
	if w.remoteDev() != base {
		t.Fatalf("dev moved to %s for the old head %s", w.remoteDev(), pr.HeadSHA)
	}
	if got := w.pendingHead("pr-1"); got != newHead {
		t.Fatalf("pending/pr-1 holds %q, want the new head %s", got, newHead)
	}
	l := readLanding(t, filepath.Join(w.cfg.RunsDir, first.Summary.Tag+".pr.json"))
	if l.Head != pr.HeadSHA || l.HeadMoved != newHead || l.DevAfter != "" || l.Result != "head-moved:"+newHead {
		t.Errorf("the landing record %+v", l)
	}
	if c := w.fake.Comments(1); len(c) != 1 || !strings.Contains(c[0], "head moved: "+newHead) {
		t.Errorf("comments %q", c)
	}

	second := w.pass()
	requireExit(t, second, ExitPushed, "pushed")
	w.git(w.remote, "merge-base", "--is-ancestor", newHead, "dev")
	if !w.doneFor("pr-1", newHead) {
		t.Errorf("done/ holds no entry for pr-1 at %s", newHead)
	}
	if got := strings.Count(w.ctlRead("review-calls"), "\n"); got != 2 || w.ctlRead("verify-n") != "2\n" {
		t.Errorf("verified %q, reviewed %d times; want the new head verified and reviewed", w.ctlRead("verify-n"), got)
	}
	if l := readLanding(t, filepath.Join(w.cfg.RunsDir, second.Summary.Tag+".pr.json")); l.Head != newHead || l.DevAfter != w.remoteDev() {
		t.Errorf("the second landing record %+v", l)
	}
}

// TestAPullRequestLandsAgainAfterNewCommitsAndNotTwiceForOneHead: done/ keeps one entry per head of a pull request; the project
// session enqueues the pull request again after new commits and the queue lands them; the head that already landed is refused.
func TestAPullRequestLandsAgainAfterNewCommitsAndNotTwiceForOneHead(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if !w.doneFor("pr-1", pr.HeadSHA) {
		t.Fatalf("done/ holds no entry for pr-1 at %s", pr.HeadSHA)
	}
	h2 := w.pushToMilestone("later.txt")
	if e := w.enqueue("o/r#1"); e.Head != h2 {
		t.Fatalf("enqueued %+v, want the new head %s", e, h2)
	}
	requireExit(t, w.pass(), ExitPushed, "pushed")
	w.git(w.remote, "merge-base", "--is-ancestor", h2, "dev")
	if !w.doneFor("pr-1", pr.HeadSHA) || !w.doneFor("pr-1", h2) {
		t.Errorf("done/ does not hold both heads of pr-1")
	}
	if _, err := w.r.EnqueuePR(t.Context(), "o/r#1"); err == nil || !strings.Contains(err.Error(), "already landed") {
		t.Errorf("enqueue of the head that landed: %v", err)
	}
	if got := w.pendingHead("pr-1"); got != "" {
		t.Errorf("the refused enqueue left pending/pr-1 = %s", got)
	}
	heads := map[string]int{}
	for _, l := range w.landings() {
		if l.DevAfter != "" {
			heads[l.Head]++
		}
	}
	if len(heads) != 2 || heads[pr.HeadSHA] != 1 || heads[h2] != 1 {
		t.Errorf("landed records per head %v", heads)
	}
}

// TestALandingWhoseMoveFailedAfterThePushIsRecordedOnce: the push landed and the record was written, but the entry could not leave
// pending (restored); the next pass writes no second record and no second comment, and moves the entry to done.
func TestALandingWhoseMoveFailedAfterThePushIsRecordedOnce(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	done := filepath.Join(w.cfg.QueueDir, "done")
	t.Cleanup(func() { _ = os.Chmod(done, 0o755) })
	w.r.hookMove = func(string) { _ = os.Chmod(done, 0o555) }
	requireExit(t, w.pass(), ExitPushed, "pushed")
	if got := w.pendingHead("pr-1"); got != pr.HeadSHA {
		t.Fatalf("the failed move left pending/pr-1 = %q", got)
	}
	if err := os.Chmod(done, 0o755); err != nil {
		t.Fatal(err)
	}
	w.r.hookMove = nil
	dev := w.remoteDev()
	// the dry run plans no run for the head that landed
	if dry, err := w.r.DryRun(t.Context(), false); err != nil || len(dry.Plan) != 0 || len(dry.Entries) != 1 || !strings.Contains(dry.Entries[0].Landed, "already landed") {
		t.Errorf("dry run %+v %v", dry, err)
	}

	rep := w.pass()
	if w.remoteDev() != dev || w.ctlRead("verify-n") != "1\n" {
		t.Fatalf("the second pass ran again: dev %s, verified %q (pass %+v)", w.remoteDev(), w.ctlRead("verify-n"), rep)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("pr-1 is not in done/ for its head (moved %q)", rep.Moved)
	}
	if !strings.Contains(strings.Join(rep.Moved, "\n"), "already landed") {
		t.Errorf("moved %q", rep.Moved)
	}
	if c := w.fake.Comments(1); len(c) != 1 {
		t.Errorf("%d comments, want the one of the landing: %q", len(c), c)
	}
	if ls := w.landings(); len(ls) != 1 || ls[0].Result != "pushed" {
		t.Errorf("landing records %+v", ls)
	}
}

// crashAfterPush undoes what a pass did after its push: the landing record and the move to done, as a pass that ended right
// after the push leaves the queue. It answers the first run's tag.
func (w *prWorld) crashAfterPush(rep Report, head string) {
	w.t.Helper()
	if err := os.Remove(filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json")); err != nil {
		w.t.Fatal(err)
	}
	moved, _ := filepath.Glob(filepath.Join(w.cfg.QueueDir, "done", "pr-1*"))
	for _, p := range moved {
		if err := os.Remove(p); err != nil {
			w.t.Fatal(err)
		}
	}
	w.pend("pr-1", head)
}

// TestALandingWithoutItsRecordGetsOneRecoveredRecord: a pass ended after the push and before the record; GitHub still shows the
// pull request open. The next pass sees the head in dev and writes one record, landed_before_record, with dev_after the dev commit
// that holds the head and the verification and review of the run files, posts its comment once and moves the entry to done.
func TestALandingWithoutItsRecordGetsOneRecoveredRecord(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)

	rep := w.pass()
	if w.remoteDev() != dev || w.ctlRead("verify-n") != "1\n" {
		t.Fatalf("the recovery pass ran again: dev %s, verified %q", w.remoteDev(), w.ctlRead("verify-n"))
	}
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v", ls)
	}
	l := ls[0]
	sum := sha256.Sum256([]byte("{\"ok\":true}\n"))
	verdict := filepath.Join(w.cfg.RunsDir, first.Summary.Tag+".review.json")
	if l.Result != "landed_before_record" || l.Number != 1 || l.Head != pr.HeadSHA || l.DevBefore != base || l.DevAfter != dev || l.Merged != dev ||
		l.Verification.Record != first.Summary.Record || l.Verification.SHA256 != hex.EncodeToString(sum[:]) || l.Verification.Status != 0 ||
		l.Review.Verdict != "accept" || l.Review.VerdictFile != verdict || l.Comment != "written" {
		t.Errorf("the recovered record %+v", l)
	}
	if rep.Landing == nil || rep.Landing.Result != "landed_before_record" {
		t.Errorf("the pass reports %+v", rep.Landing)
	}
	c := w.fake.Comments(1)
	if len(c) != 2 || !strings.Contains(c[1], "result: landed_before_record") || !strings.Contains(c[1], "dev: "+base+" -> "+dev) {
		t.Fatalf("comments %q", c)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("pr-1 is not in done/ (moved %q)", rep.Moved)
	}
	// once: the same head pending again is moved to done with no record and no comment
	w.pend("pr-1", pr.HeadSHA)
	w.pass()
	if len(w.landings()) != 1 || len(w.fake.Comments(1)) != 2 || w.pendingHead("pr-1") != "" {
		t.Errorf("a second recovery: %d records, %d comments, pending %q", len(w.landings()), len(w.fake.Comments(1)), w.pendingHead("pr-1"))
	}
}

// TestAMergedPullRequestWithoutItsRecordGetsOneRecoveredRecord: GitHub shows the pull request merged (dev holds its head) and no
// record and no run file of the landing is left; the pass writes one recovered record that says the verification and the review
// were not recorded, comments once and moves the entry to done.
func TestAMergedPullRequestWithoutItsRecordGetsOneRecoveredRecord(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.crashAfterPush(first, pr.HeadSHA)
	for _, ext := range []string{".summary.json", ".out", ".json", ".review.json"} {
		_ = os.Remove(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+ext))
	}
	w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true })

	rep := w.pass()
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v (moved %q)", ls, rep.Moved)
	}
	l := ls[0]
	if l.Result != "landed_before_record" || l.Head != pr.HeadSHA || l.DevAfter != dev || l.DevBefore != base || l.Verification.Record != "" ||
		l.Verification.Status != -1 || !strings.Contains(l.Verification.Note, "not recorded") || !strings.Contains(l.Review.Note, "not recorded") {
		t.Errorf("the recovered record %+v", l)
	}
	if c := w.fake.Comments(1); len(c) != 2 || !strings.Contains(c[1], "result: landed_before_record") || !strings.Contains(c[1], "not recorded") {
		t.Errorf("comments %q", c)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.remoteDev() != dev {
		t.Errorf("pr-1 not in done/ or dev moved (moved %q)", rep.Moved)
	}
}
