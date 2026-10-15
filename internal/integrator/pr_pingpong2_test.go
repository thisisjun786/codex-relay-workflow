package integrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1413 ping-pong round 2: (1) the push does not start unless the pushing marker is on disk; (2) a pass whose only work is a
// successful comment repost is a successful pass.

// TestNoPushWithoutAWrittenPushingMarker: runs_dir refuses the marker write (here <tag>.summary.json is a directory, so the write
// fails whoever runs the test). Nothing is pushed, the pass reports the failure and the entry stays pending.
func TestNoPushWithoutAWrittenPushingMarker(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	base := w.remoteDev()

	delegate := w.r.Exec
	blocked := false
	w.r.Exec = func(ctx context.Context, c Cmd) (int, error) {
		// the verification has run: the second fetch is right before the marker is written
		isFetch := false
		for _, a := range c.Argv {
			isFetch = isFetch || a == "fetch"
		}
		if isFetch && !blocked && w.ctlRead("verify-n") == "1\n" {
			logs, _ := filepath.Glob(filepath.Join(w.cfg.RunsDir, "*.log"))
			for _, l := range logs {
				tag := strings.TrimSuffix(filepath.Base(l), ".log")
				if strings.Contains(tag, ".") {
					continue
				}
				if err := os.Mkdir(filepath.Join(w.cfg.RunsDir, tag+".summary.json"), 0o775); err != nil {
					t.Fatal(err)
				}
				blocked = true
			}
		}
		return delegate(ctx, c)
	}
	rep := w.pass()
	if !blocked {
		t.Fatal("the marker write was never blocked")
	}
	if rep.Exit != ExitUsage || rep.Result != "error" || !strings.Contains(rep.Error, "pushing marker") {
		t.Fatalf("pass = exit %d result %q error %q, want exit %d error naming the pushing marker", rep.Exit, rep.Result, rep.Error, ExitUsage)
	}
	if dev := w.remoteDev(); dev != base {
		t.Fatalf("dev moved to %s although the marker could not be written (was %s)", dev, base)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("pr-1 is not pending (pending %q)", w.pendingHead("pr-1"))
	}
	if ls := w.landings(); len(ls) != 0 {
		t.Errorf("landing records %+v", ls)
	}
}

// TestASuccessfulCommentRepostIsASuccessfulPass: the only pending entry has a landing record whose comment was not written yet. The
// pass posts the comment, moves the entry to done/ and reports comment-reposted with status 0, not nothing-to-integrate (5).
func TestASuccessfulCommentRepostIsASuccessfulPass(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	w.interruptBeforeComment(first, pr.HeadSHA, "not written yet")

	rep := w.pass()
	requireExit(t, rep, ExitPushed, resultReportCommentReposted)
	if rep.Error != "" || len(rep.Held) != 0 {
		t.Errorf("error %q, held %q", rep.Error, rep.Held)
	}
	if c := w.fake.Comments(1); len(c) != 1 {
		t.Errorf("comments %q", c)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" || w.remoteDev() != dev {
		t.Errorf("pr-1 is not in done/ (pending %q) or dev moved", w.pendingHead("pr-1"))
	}
}
