// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/domain/review.go and internal/domain/result.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package review

import _ "embed" // schema_v1.json

// SchemaV1 is the value of an artifact's schema field.
const SchemaV1 = "crw-independent-review/1"

// Status is the outcome of a review run: ACR's ReviewStatus (completed, failed, interrupted) as the three states CRW records.
type Status string

// The three statuses. Reason is empty exactly when the status is complete.
const (
	StatusComplete    Status = "complete"
	StatusPartial     Status = "partial"
	StatusUnavailable Status = "unavailable"
)

// ReviewerCounts is ACR's ReviewStats as counts: Run reviewers were started and Failed of them gave no result, of which TimedOut
// timed out and AuthFailed failed to authenticate (the other failures are in Failed only); Run-Failed returned a result.
type ReviewerCounts struct {
	Run        int `json:"run"`
	Failed     int `json:"failed"`
	TimedOut   int `json:"timedOut"`
	AuthFailed int `json:"authFailed"`
}

// DiffStats describes the base...head diff that was reviewed.
type DiffStats struct {
	Files     int `json:"files"`
	Additions int `json:"additions"`
	Deletions int `json:"deletions"`
}

// Artifact is schema v1 of the independent review's output; the contract is in the package documentation. Times are RFC 3339.
type Artifact struct {
	Schema      string         `json:"schema"`
	Tool        string         `json:"tool"`
	ToolVersion string         `json:"toolVersion"`
	Model       string         `json:"model"`
	Effort      string         `json:"effort"`
	AgyVersion  string         `json:"agyVersion"` // "unknown" when the version could not be read
	Base        string         `json:"base"`
	Head        string         `json:"head"`
	PatchID     string         `json:"patchId"`
	Diff        DiffStats      `json:"diff"`
	Reviewers   ReviewerCounts `json:"reviewers"`
	Status      Status         `json:"status"`
	Reason      string         `json:"reason"`
	StartedAt   string         `json:"startedAt"`
	FinishedAt  string         `json:"finishedAt"`
	Findings    []Finding      `json:"findings"`
	Dropped     []Drop         `json:"dropped"`
}

//go:embed schema_v1.json
var schemaV1 []byte

// SchemaV1JSON returns the JSON Schema text of schema v1 for readers outside Go. The Go validator is the authority (it states rules
// JSON Schema cannot) and a test keeps the two in step.
func SchemaV1JSON() []byte { return append([]byte(nil), schemaV1...) }

var allStatuses = []Status{StatusComplete, StatusPartial, StatusUnavailable}
