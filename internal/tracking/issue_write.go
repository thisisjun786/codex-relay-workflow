package tracking

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// CRW-1549: the write side of the adapter for issues and milestones. OpenIssue and OpenMilestone are
// on GitHub only — Tracker is not widened, so the fakes that implement it need no new methods. Every
// write goes through request, whose publicJSON scrub edits or refuses the public text fields before
// the request leaves.

// NewIssue is what OpenIssue sends: a title and body, a milestone number (0 is none) and labels.
type NewIssue struct {
	Title, Body string
	// Milestone is the number of the milestone the issue goes in, 0 for none.
	Milestone int
	Labels    []string
}

// OpenIssue posts one issue to the tracker's repository.
func (g *GitHub) OpenIssue(ctx context.Context, in NewIssue) (Issue, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Issue{}, fmt.Errorf("%w: the title is blank", ErrInvalidIssue)
	}
	if in.Milestone < 0 {
		return Issue{}, fmt.Errorf("%w: milestone %d", ErrInvalidIssue, in.Milestone)
	}
	send := map[string]any{"title": title, "body": in.Body}
	if in.Milestone > 0 {
		send["milestone"] = in.Milestone
	}
	if len(in.Labels) > 0 {
		send["labels"] = in.Labels
	}
	var a apiIssue
	if _, err := g.request(ctx, "tracking: open issue in "+g.repo, http.MethodPost, g.repoURL("/issues", nil), send, &a); err != nil {
		return Issue{}, err
	}
	return g.issueFrom(a), nil
}

// NewMilestone is what OpenMilestone sends: a title and a description.
type NewMilestone struct {
	Title, Description string
}

// apiMilestone is the part of a GitHub milestone the adapter reads.
type apiMilestone struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	OpenIssues   int    `json:"open_issues"`
	ClosedIssues int    `json:"closed_issues"`
}

// OpenMilestone posts one milestone to the tracker's repository.
func (g *GitHub) OpenMilestone(ctx context.Context, in NewMilestone) (Milestone, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" {
		return Milestone{}, fmt.Errorf("%w: the title is blank", ErrInvalidMilestone)
	}
	var m apiMilestone
	if _, err := g.request(ctx, "tracking: open milestone in "+g.repo, http.MethodPost, g.repoURL("/milestones", nil),
		map[string]string{"title": title, "description": in.Description}, &m); err != nil {
		return Milestone{}, err
	}
	return Milestone{
		Ref: Ref{Repo: g.repo, Number: m.Number}, Title: m.Title, Description: m.Description, State: State(m.State),
		OpenIssues: m.OpenIssues, ClosedIssues: m.ClosedIssues,
	}, nil
}
