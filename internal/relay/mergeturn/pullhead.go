package mergeturn

import "context"

// PullRequestHeadReading is where a pull request's head points on the forge now.
type PullRequestHeadReading struct {
	Repository string
	Number     int64
	SHA        string
	Source     string
}

// PullRequestHeadReader reads the head of one pull request.
type PullRequestHeadReader interface {
	PullRequestHead(ctx context.Context, repository string, number int64) (PullRequestHeadReading, error)
}

// PullRequestHead is not implemented yet: it reads nothing.
func (r TargetReader) PullRequestHead(ctx context.Context, repository string, number int64) (PullRequestHeadReading, error) {
	return PullRequestHeadReading{}, unreadable("pull request heads are not read")
}
