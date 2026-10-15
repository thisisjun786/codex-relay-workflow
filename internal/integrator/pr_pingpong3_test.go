package integrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking"
)

// refusingComment is a pull request source that reads and looks up as the real one and refuses to post a comment.
type refusingComment struct{ PullRequestSource }

func (refusingComment) Comment(context.Context, tracking.Ref, string, string) error {
	return errors.New("the host refuses the comment")
}

// CRW-1413 ping-pong round 2 follow-on: a comment repost that the pull request host refuses is not a posted comment.

// TestARefusedCommentRepostIsNotReportedAsPosted: the only pending entry has a landing record whose comment was not written yet, and
// the host refuses the comment (the lookup works and finds nothing, the post is refused). The record keeps the refusal and the entry goes to done/, as
// before, but the pass does not report comment-reposted: no comment went up, so it is nothing-to-integrate (5).
func TestARefusedCommentRepostIsNotReportedAsPosted(t *testing.T) {
	w := newPRWorld(t, true)
	pr := w.milestone("10")
	w.enqueue("o/r#1")
	first := w.pass()
	requireExit(t, first, ExitPushed, "pushed")
	path := w.interruptBeforeComment(first, pr.HeadSHA, "not written yet")
	w.r.PullRequests = refusingComment{w.gh}

	rep := w.pass()
	requireExit(t, rep, ExitNothingMerged, "nothing-to-integrate")
	if c := w.fake.Comments(1); len(c) != 0 {
		t.Errorf("comments %q", c)
	}
	if l := readLanding(t, path); !strings.HasPrefix(l.Comment, commentRefused) {
		t.Errorf("the comment of the record is %q, want the refusal", l.Comment)
	}
	if !w.doneFor("pr-1", pr.HeadSHA) || w.pendingHead("pr-1") != "" {
		t.Errorf("pr-1 is not in done/ (pending %q)", w.pendingHead("pr-1"))
	}
}
