package tracking

import (
	"context"
	"net/http"
)

// GitHub is a Tracker over the GitHub REST API for one repository.
type GitHub struct{}

var _ Tracker = (*GitHub)(nil)

// Option changes a GitHub made by NewGitHub.
type Option func(*GitHub)

// WithBaseURL sets the API root, "https://api.github.com" by default.
func WithBaseURL(u string) Option { return func(*GitHub) {} }

// WithHTTPClient sets the client the requests go through.
func WithHTTPClient(c *http.Client) Option { return func(*GitHub) {} }

// NewGitHub makes a Tracker for the repository "owner/name".
func NewGitHub(repo string, tokens TokenSource, opts ...Option) (*GitHub, error) {
	return nil, errNotImplemented
}

func (g *GitHub) Milestone(ctx context.Context, ref Ref) (Milestone, []Issue, error) {
	return Milestone{}, nil, errNotImplemented
}
func (g *GitHub) Issue(ctx context.Context, ref Ref) (Issue, error) {
	return Issue{}, errNotImplemented
}
func (g *GitHub) Comment(ctx context.Context, ref Ref, header, text string) error {
	return errNotImplemented
}
func (g *GitHub) Close(ctx context.Context, ref Ref, reason CloseReason) error {
	return errNotImplemented
}
func (g *GitHub) AddLabel(ctx context.Context, ref Ref, label string) error {
	return errNotImplemented
}
