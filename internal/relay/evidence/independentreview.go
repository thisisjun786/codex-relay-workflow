package evidence

import (
	"errors"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
)

// IndependentReviewMember is the member of a handoff record, and of a receipt, that carries the
// child's statement about its independent code review.
const IndependentReviewMember = "independentReview"

// The warning codes. A warning is a reading the parent should look at; none of them is a problem
// in the sense of Problem lists that decide a verdict, and none can block a merge.
const (
	ReviewAbsent             = "independent_review_absent"
	ReviewMalformed          = "independent_review_malformed"
	ReviewUnreadable         = "independent_review_unreadable"
	ReviewSHA256Mismatch     = "independent_review_sha256_mismatch"
	ReviewArtifactInvalid    = "independent_review_artifact_invalid"
	ReviewStatusDiffers      = "independent_review_status_differs"
	ReviewHeadDiffers        = "independent_review_head_differs"
	ReviewDispositionMissing = "independent_review_disposition_missing"
	ReviewDispositionUnknown = "independent_review_disposition_unknown"
)

// reviewArtifactCap is the most bytes of an artifact file that are read.
var reviewArtifactCap int64 = 8 << 20

// ArtifactReader reads the artifact file an item names.
type ArtifactReader func(path string) ([]byte, error)

// ReviewCoverage is what the coverage reading of one record says.
type ReviewCoverage struct {
	Stated   bool
	Warnings []Problem
}

// IndependentReviewCoverage is not implemented yet.
func IndependentReviewCoverage(head string, record any, read ArtifactReader) ReviewCoverage {
	_ = review.SchemaV1
	return ReviewCoverage{}
}

var (
	errArtifactNotRegular = errors.New("not a regular file")
	errArtifactTooLarge   = errors.New("larger than the cap")
)

// readArtifactFile is not implemented yet.
func readArtifactFile(path string) ([]byte, error) { return nil, errors.New("not implemented") }

// openRegular is not implemented yet.
func openRegular(path string) (*os.File, error) { return nil, errors.New("not implemented") }
