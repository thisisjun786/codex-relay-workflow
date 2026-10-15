package integrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1413 item 2: a landing record whose comment is "not written yet" (the pass ended between the record and the comment) gets its
// comment on the next pass that finds the head landed. A comment that is "written" or was refused is not posted again, and
// --enqueue-pr and --dry-run, which only read, post nothing.

// interruptComment rewrites the landing record of the first run as an interrupted pass leaves it (the comment not written yet),
// takes the entry out of done/ and makes the head pending again. It answers the record's path.
func (w *prWorld) interruptComment(rep Report, head, comment string) string {
	w.t.Helper()
	path := filepath.Join(w.cfg.RunsDir, rep.Summary.Tag+".pr.json")
	l := readLanding(w.t, path)
	l.Comment = comment
	w.r.writeLanding(&l)
	moved, _ := filepath.Glob(filepath.Join(w.cfg.QueueDir, "done", "pr-1*"))
	for _, p := range moved {
		if err := os.Remove(p); err != nil {
			w.t.Fatal(err)
		}
	}
	w.pend("pr-1", head)
	return path
}

// TestAnInterruptedLandingCommentIsPostedOnTheNextPass: a real pushed run whose record says "not written yet" gets its comment
// from the next pass, which verifies nothing, moves nothing in dev and moves the entry to done/.
func TestAnInterruptedLandingCommentIsPostedOnTheNextPass(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	if c := w.fake.Comments(1); len(c) != 1 {
		t.Fatalf("comments after the run %q", c)
	}
	path := w.interruptComment(first, pr.HeadSHA, "not written yet")

	w.pass()
	c := w.fake.Comments(1)
	if len(c) != 2 || !strings.HasPrefix(c[1], "[dev queue · "+first.Summary.Tag+"]") || !strings.Contains(c[1], "result: pushed") {
		t.Fatalf("comments %q", c)
	}
	if l := readLanding(t, path); l.Comment != "written" || l.Result != "pushed" {
		t.Errorf("the record after the repost %+v", l)
	}
	if ls := w.landings(); len(ls) != 1 {
		t.Errorf("landing records %+v", ls)
	}
	if w.ctlRead("verify-n") != "1\n" || w.remoteDev() != dev {
		t.Errorf("the repost pass ran again: verified %q, dev %s (was %s)", w.ctlRead("verify-n"), w.remoteDev(), dev)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("pr-1 is not in done/ (pending %q)", w.pendingHead("pr-1"))
	}
	// once: the same head pending again posts nothing more
	w.pend("pr-1", pr.HeadSHA)
	w.pass()
	if n := len(w.fake.Comments(1)); n != 2 {
		t.Errorf("a second pass posted again: %d comments", n)
	}
}

// TestAWrittenOrRefusedCommentIsNotPostedAgain: only "not written yet" is reposted; "written" and a refusal stay as they are.
func TestAWrittenOrRefusedCommentIsNotPostedAgain(t *testing.T) {
	for _, comment := range []string{"written", "not written: boom"} {
		t.Run(comment, func(t *testing.T) {
			w := newPRWorld(t, true)
			pr := w.milestone("10")
			w.enqueue("o/r#1")
			first := w.pass()
			requireExit(t, first, ExitPushed, "pushed")
			path := w.interruptComment(first, pr.HeadSHA, comment)

			w.pass()
			if c := w.fake.Comments(1); len(c) != 1 {
				t.Errorf("comments %q", c)
			}
			if l := readLanding(t, path); l.Comment != comment {
				t.Errorf("the comment of the record is %q, want %q", l.Comment, comment)
			}
			if !w.doneFor("pr-1", pr.HeadSHA) {
				t.Error("pr-1 is not in done/")
			}
		})
	}
}

// TestEnqueueAndDryRunPostNoLandingComment: --enqueue-pr and --dry-run only read the landing; a "not written yet" record keeps its
// state and the pull request gets no comment.
func TestEnqueueAndDryRunPostNoLandingComment(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	path := w.interruptComment(first, pr.HeadSHA, "not written yet")

	rep, err := w.r.DryRun(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(rep.Entries) != 1 || rep.Entries[0].Landed == "" {
		t.Errorf("the dry run reports %+v", rep.Entries)
	}
	if _, err := w.r.EnqueuePR(context.Background(), "o/r#1"); err == nil || !strings.Contains(err.Error(), "already landed") {
		t.Errorf("enqueue of a landed head: %v", err)
	}
	if c := w.fake.Comments(1); len(c) != 1 {
		t.Errorf("comments %q", c)
	}
	if l := readLanding(t, path); l.Comment != "not written yet" {
		t.Errorf("the comment of the record is %q", l.Comment)
	}
}
