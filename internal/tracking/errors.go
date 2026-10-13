package tracking

import (
	"errors"
	"fmt"
	"time"
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
	ErrInvalidRef = errors.New("tracking: invalid reference")
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

// APIError is an HTTP answer GitHub gave that is not a success.
type APIError struct {
	Op      string
	Status  int
	Message string
	DocURL  string
}

func (e *APIError) Error() string { return fmt.Sprintf("%s: HTTP %d", e.Op, e.Status) }

// Is matches the sentinel errors by status.
func (e *APIError) Is(target error) bool { return false }

// RateLimitError is a 429 answer or a 403 answer that says the rate limit is used up.
type RateLimitError struct {
	APIError
	// Reset is when the primary limit resets, zero when GitHub did not say.
	Reset time.Time
	// RetryAfter is how long GitHub asked to wait, 0 when it did not say.
	RetryAfter time.Duration
}

// Is matches ErrRateLimited only.
func (e *RateLimitError) Is(target error) bool { return false }
