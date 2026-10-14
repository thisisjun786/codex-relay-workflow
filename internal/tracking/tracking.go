// Package tracking is the work tracking adapter: one interface, Tracker, over the place CRW keeps
// its milestones and issues, and one implementation, GitHub, over the GitHub REST API.
//
// An issue or milestone is named "<tracking repo>#N", for example "thisisjun786/crw-work#49" (Ref).
// A Tracker reads a milestone with its issues, reads an issue with its labels, state, sub-issues
// and the "Blocked by:" lines of its body, writes a comment whose first line is the caller's
// "[layer · session]" header, closes an issue as completed or not planned, and adds a label. Its pull request side
// (PullRequests) opens a pull request from a milestone integration branch naming the issues it carries, and reads a pull
// request's state and the checks of its head.
//
// The adapter has no opinion on who may write: the caller decides that only the owning session
// writes and an executor does not. It holds no caller, no relay, flow or manage state and imports
// none of those packages; the repository comes in as a parameter and the token comes from a
// TokenSource, so a test runs the whole adapter against a fake GitHub.
package tracking

import (
	"context"
	"fmt"
	"strings"
)

// Tracker is the work tracking adapter. Every method takes the identifier of what it touches as a
// Ref; an implementation refuses a Ref of another repository than the one it was made for.
type Tracker interface {
	// Milestone reads the milestone ref and every issue of it, open and closed. A pull request is
	// not an issue and is left out. The issues carry their body, labels, state and BlockedBy; their
	// SubIssues are filled by Issue only.
	Milestone(ctx context.Context, ref Ref) (Milestone, []Issue, error)
	// Issue reads one issue: body, labels, state, sub-issues and the "Blocked by:" lines of the body.
	Issue(ctx context.Context, ref Ref) (Issue, error)
	// Comment writes a comment on the issue. The comment's first line is header, which has the form
	// "[layer · session]"; text follows after a blank line.
	Comment(ctx context.Context, ref Ref, header, text string) error
	// Close closes the issue with the reason: completed or not planned.
	Close(ctx context.Context, ref Ref, reason CloseReason) error
	// AddLabel adds the label to the issue and keeps its other labels.
	AddLabel(ctx context.Context, ref Ref, label string) error
}

// State is whether an issue or milestone is open.
type State string

const (
	StateOpen   State = "open"
	StateClosed State = "closed"
)

// CloseReason is why an issue is closed.
type CloseReason string

const (
	// Completed closes an issue as done.
	Completed CloseReason = "completed"
	// NotPlanned closes an issue as not going to be done.
	NotPlanned CloseReason = "not_planned"
)

// Milestone is a milestone of the tracking repository.
type Milestone struct {
	Ref          Ref
	Title        string
	Description  string
	State        State
	OpenIssues   int
	ClosedIssues int
}

// Issue is an issue of a tracking repository.
type Issue struct {
	Ref   Ref
	URL   string
	Title string
	Body  string
	State State
	// CloseReason is why a closed issue was closed; it is empty for an open issue and for a closed
	// issue the tracker records no reason for.
	CloseReason CloseReason
	Labels      []string
	// Milestone is the number of the issue's milestone, 0 for none.
	Milestone int
	// SubIssues are the issue's sub-issues. Only Tracker.Issue fills it.
	SubIssues []SubIssue
	// BlockedBy are the issues named on the "Blocked by:" lines of Body, in order, once each.
	BlockedBy []Ref
}

// SubIssue is a sub-issue of an issue; it can live in another repository.
type SubIssue struct {
	Ref   Ref
	Title string
	State State
}

// checkHeader accepts one line "[layer · session]": brackets around two non-blank parts split by the
// middle dot, with no bracket or line break inside.
func checkHeader(header string) error {
	inner, ok := strings.CutPrefix(header, "[")
	if ok {
		inner, ok = strings.CutSuffix(inner, "]")
	}
	layer, session, dot := strings.Cut(inner, "·")
	if !ok || !dot || strings.ContainsAny(inner, "[]\r\n") ||
		strings.TrimSpace(layer) == "" || strings.TrimSpace(session) == "" {
		return fmt.Errorf("%w: %q is not one line [layer · session]", ErrInvalidHeader, header)
	}
	return nil
}
