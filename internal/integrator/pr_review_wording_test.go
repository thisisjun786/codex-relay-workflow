package integrator

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	trackingfake "github.com/thisisjun786/codex-relay-workflow/internal/tracking/trackingfake"
)

// recoveredRecord lands one pull request, undoes what a pass did after its push, optionally removes the run files, and answers
// the record and the comment of the recovery pass.
func recoveredRecord(t *testing.T, review, removeRunFiles bool) (Landing, string) {
	t.Helper()
	w := newPRWorld(t, review)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	w.crashAfterPush(first, pr.HeadSHA)
	if removeRunFiles {
		for _, ext := range []string{".summary.json", ".out", ".json", ".review.json"} {
			_ = os.Remove(filepath.Join(w.cfg.RunsDir, first.Summary.Tag+ext))
		}
		w.fake.SetPull(1, func(p *trackingfake.Pull) { p.State, p.Merged = "closed", true })
	}
	w.pass()
	ls := w.landings()
	if len(ls) != 1 {
		t.Fatalf("landing records %+v", ls)
	}
	c := w.fake.Comments(1)
	if len(c) != 2 {
		t.Fatalf("comments %q", c)
	}
	return ls[0], c[1]
}

// TestARecoveredLandingOfAQueueWithoutAReviewCommandSaysNotConfigured: the queue has no review command, so the run left no
// verdict file and the recovered record says "review not configured", as the landing of a normal pass does.
func TestARecoveredLandingOfAQueueWithoutAReviewCommandSaysNotConfigured(t *testing.T) {
	l, comment := recoveredRecord(t, false, false)
	if l.Result != "landed_before_record" || l.Review.Configured || l.Review.Note != "review not configured" {
		t.Errorf("the recovered review %+v", l.Review)
	}
	if !strings.Contains(comment, "review: not configured") || strings.Contains(comment, "no readable verdict") {
		t.Errorf("comment %q", comment)
	}
}

// TestARecoveredLandingWithoutRunFilesOfAQueueWithoutAReviewCommandSaysNotConfigured is the same when no run file is left.
func TestARecoveredLandingWithoutRunFilesOfAQueueWithoutAReviewCommandSaysNotConfigured(t *testing.T) {
	l, comment := recoveredRecord(t, false, true)
	if l.Result != "landed_before_record" || l.Review.Configured || l.Review.Note != "review not configured" {
		t.Errorf("the recovered review %+v", l.Review)
	}
	if !strings.Contains(comment, "review: not configured") || strings.Contains(comment, "review: not recorded") {
		t.Errorf("comment %q", comment)
	}
}

// TestARecoveredLandingOfAConfiguredQueueWithoutAVerdictStillSaysNotRecorded guards the configured case.
func TestARecoveredLandingOfAConfiguredQueueWithoutAVerdictStillSaysNotRecorded(t *testing.T) {
	l, comment := recoveredRecord(t, true, true)
	if !l.Review.Configured || !strings.HasPrefix(l.Review.Note, "not recorded") {
		t.Errorf("the recovered review %+v", l.Review)
	}
	if !strings.Contains(comment, "review: not recorded") || strings.Contains(comment, "not configured") {
		t.Errorf("comment %q", comment)
	}
}
