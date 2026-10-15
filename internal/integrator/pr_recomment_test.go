package integrator

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
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

// interruptBeforeComment is interruptComment for a pass that died before the comment call: the comment of the run is not on the
// pull request either (the fake drops it), so the next pass has to post it.
func (w *prWorld) interruptBeforeComment(rep Report, head, comment string) string {
	w.t.Helper()
	w.fake.DropComments(1)
	return w.interruptComment(rep, head, comment)
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
	path := w.interruptBeforeComment(first, pr.HeadSHA, "not written yet")

	w.pass()
	c := w.fake.Comments(1)
	if len(c) != 1 || !strings.HasPrefix(c[0], "[dev queue · "+first.Summary.Tag+"]") || !strings.Contains(c[0], "result: pushed") {
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
	if n := len(w.fake.Comments(1)); n != 1 {
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

// TestACommentThatWentUpBeforeTheCrashIsNotPostedAgain: the pass died between the comment call and the second write of the record, so
// the record says "not written yet" for a comment that is on the pull request. The next pass finds the comment by its header, posts
// nothing, writes "written" and moves the entry to done/.
func TestACommentThatWentUpBeforeTheCrashIsNotPostedAgain(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	path := w.interruptComment(first, pr.HeadSHA, "not written yet")

	w.pass()
	c := w.fake.Comments(1)
	if len(c) != 1 || !strings.HasPrefix(c[0], "[dev queue · "+first.Summary.Tag+"]") {
		t.Fatalf("comments %q, want the one of the run alone", c)
	}
	if l := readLanding(t, path); l.Comment != "written" {
		t.Errorf("the record after the pass %+v", l)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("pr-1 is not in done/ (pending %q)", w.pendingHead("pr-1"))
	}
}

// TestAFoundCommentIsReportedAsNothingToIntegrate: a comment found on the pull request is not a comment the pass posted, so the pass
// that only finds it reports nothing-to-integrate (5), not comment-reposted; the entry is done and the record says written.
func TestAFoundCommentIsReportedAsNothingToIntegrate(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	dev := w.remoteDev()
	path := w.interruptComment(first, pr.HeadSHA, "not written yet")

	rep := w.pass()
	requireExit(t, rep, ExitNothingMerged, "nothing-to-integrate")
	if rep.Error != "" || len(rep.Held) != 0 {
		t.Errorf("error %q, held %q", rep.Error, rep.Held)
	}
	if n := len(w.fake.Comments(1)); n != 1 {
		t.Errorf("%d comments, want 1", n)
	}
	if l := readLanding(t, path); l.Comment != "written" {
		t.Errorf("the record %+v", l)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" || w.remoteDev() != dev {
		t.Errorf("pr-1 is not in done/ (pending %q) or dev moved", w.pendingHead("pr-1"))
	}
}

// TestAFailedCommentLookupHoldsTheEntryAndPostsNothing: when the pull request cannot be asked for its comments (a client with a wrong
// token), the pass does not guess: the entry stays pending with the reason, the record is untouched and nothing is posted.
func TestAFailedCommentLookupHoldsTheEntryAndPostsNothing(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	path := w.interruptComment(first, pr.HeadSHA, "not written yet")
	refusing, err := tracking.NewGitHub("o/r", tracking.StaticToken("wrong"), tracking.WithBaseURL(w.fake.URL))
	if err != nil {
		t.Fatal(err)
	}
	w.r.PullRequests = refusing

	rep := w.pass()
	if len(rep.Held) != 1 || !strings.HasPrefix(rep.Held[0], "pr-1: ") || !strings.Contains(rep.Held[0], "could not be looked up on the pull request") {
		t.Errorf("held %q", rep.Held)
	}
	if w.pendingHead("pr-1") != pr.HeadSHA || w.doneFor("pr-1", pr.HeadSHA) {
		t.Errorf("the entry left pending (pending %q, done %v)", w.pendingHead("pr-1"), w.doneFor("pr-1", pr.HeadSHA))
	}
	if l := readLanding(t, path); l.Comment != "not written yet" {
		t.Errorf("the record changed: %+v", l)
	}
	if n := len(w.fake.Comments(1)); n != 1 {
		t.Errorf("%d comments, want the one of the run", n)
	}

	w.r.PullRequests = w.gh
	w.pass()
	if l := readLanding(t, path); l.Comment != "written" {
		t.Errorf("the record once the lookup works %+v", l)
	}
	if n := len(w.fake.Comments(1)); n != 1 {
		t.Errorf("%d comments after the lookup works, want 1", n)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) {
		t.Error("the entry is not done once the lookup works")
	}
}

// TestAFailedFirstRecordWriteStillPostsTheCommentOnce guards the fallback of recordLanding: when the FIRST write of the record
// fails nothing was posted yet, so the comment goes up once, with no lookup in between.
func TestAFailedFirstRecordWriteStillPostsTheCommentOnce(t *testing.T) {
	w := newPRWorld(t, true)
	w.milestone("10")
	w.enqueue("o/r#1")
	// the run reserves the tag its clock gives: the record of that tag cannot be written
	if err := os.MkdirAll(w.cfg.RunsDir, 0o775); err != nil {
		t.Fatal(err)
	}
	w.refuseRecordWrite(w.cfg.TagPrefix + w.now.Add(time.Minute).Format("0102150405"))

	w.pass()
	c := w.fake.Comments(1)
	if len(c) != 1 || !strings.HasPrefix(c[0], "[dev queue · ") || !strings.Contains(c[0], "result: pushed") {
		t.Errorf("comments %q, want exactly the one of the run", c)
	}
	if ls := w.landings(); len(ls) != 0 {
		t.Errorf("a record was written although its write was refused: %+v", ls)
	}
}
