package tracking

import (
	"errors"
	"fmt"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/ref"
)

var (
	// ErrNotFound matches an error for a 404 (or 410) answer: the issue, milestone or repository does
	// not exist, or the token cannot see it.
	ErrNotFound = errors.New("tracking: not found")
	// ErrUnauthorized matches an error for a 401 answer: the token is missing, expired or revoked.
	ErrUnauthorized = errors.New("tracking: unauthorized")
	// ErrForbidden matches an error for a 403 answer that is not a rate limit: the token lacks the
	// permission for the operation.
	ErrForbidden = errors.New("tracking: forbidden")
	// ErrRateLimited matches a *RateLimitError: GitHub refused the request for the rate limit.
	ErrRateLimited = errors.New("tracking: rate limited")

	// ErrInvalidRef is the error of a Ref that is not "<owner>/<name>#N" with N above 0.
	ErrInvalidRef = ref.ErrInvalid
	// ErrOtherRepository is the error of a Ref of another repository than the tracker's.
	ErrOtherRepository = errors.New("tracking: reference is in another repository")
	// ErrInvalidHeader is the error of a comment header that is not one line "[layer · session]".
	ErrInvalidHeader = errors.New("tracking: invalid comment header")
	// ErrInvalidReason is the error of a close reason that is neither Completed nor NotPlanned.
	ErrInvalidReason = errors.New("tracking: invalid close reason")
	// ErrInvalidLabel is the error of an empty label or one over several lines.
	ErrInvalidLabel = errors.New("tracking: invalid label")
	// ErrNotIssue is the error of reading a pull request as an issue.
	ErrNotIssue = errors.New("tracking: not an issue")
)

// APIError is an HTTP answer GitHub gave that is not a success. Message is GitHub's own text with
// the token, should it echo it, removed.
type APIError struct {
	Op      string
	Status  int
	Message string
	DocURL  string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("%s: HTTP %d", e.Op, e.Status)
	}
	return fmt.Sprintf("%s: HTTP %d: %s", e.Op, e.Status, e.Message)
}

// Is matches ErrNotFound (404, 410), ErrUnauthorized (401) and ErrForbidden (403) by status.
func (e *APIError) Is(target error) bool {
	switch target {
	case ErrNotFound:
		return e.Status == 404 || e.Status == 410
	case ErrUnauthorized:
		return e.Status == 401
	case ErrForbidden:
		return e.Status == 403
	}
	return false
}

// RateLimitError is a 429 answer, or a 403 answer that says the rate limit is used up. It matches
// ErrRateLimited and nothing else.
type RateLimitError struct {
	APIError
	// Reset is when the primary limit resets, zero when GitHub did not say.
	Reset time.Time
	// RetryAfter is how long GitHub asked to wait, 0 when it did not say.
	RetryAfter time.Duration
}

func (e *RateLimitError) Error() string {
	s := fmt.Sprintf("%s: HTTP %d: rate limited", e.Op, e.Status)
	if e.Message != "" {
		s += ": " + e.Message
	}
	return s
}

// Is matches ErrRateLimited only.
func (e *RateLimitError) Is(target error) bool { return target == ErrRateLimited }
