// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/store/adjudication.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package adjudication

import "github.com/thisisjun786/codex-relay-workflow/internal/review"

const Schema = "crw-review-adjudication/1"

type Source string

const (
	Independent Source = "crw_review"
	Devin       Source = "devin"
	Codex       Source = "codex"
)

type Verdict string

const (
	Correct       Verdict = "correct"
	FalsePositive Verdict = "false_positive"
	Minor         Verdict = "minor"
)

// PR includes the reviewed head, so a model comparison cannot silently mix revisions.
type PR struct {
	Repository string `json:"repository"`
	Number     int    `json:"number"`
	Head       string `json:"head"`
}

type Reviewer struct {
	Source Source `json:"source"`
	Model  string `json:"model"`
}

type Finding struct {
	ID       string       `json:"id"`
	File     string       `json:"file"`
	Line     int          `json:"line"`
	Title    string       `json:"title"`
	Grade    review.Grade `json:"grade"`
	Security bool         `json:"security"`
}

// Call preserves the original counters independently of whether measurement is known.
type Call struct {
	Record       review.CallRecord `json:"record"`
	ElapsedKnown bool              `json:"elapsedKnown"`
	TokensKnown  bool              `json:"tokensKnown"`
}

type Run struct {
	ID string `json:"id"`
	PR PR     `json:"pr"`
	Reviewer
	ArtifactSHA256 string    `json:"artifactSha256"`
	Status         string    `json:"status"`
	Reason         string    `json:"reason"`
	Reviewers      *int      `json:"reviewers"`
	Calls          []Call    `json:"calls"`
	Findings       []Finding `json:"findings"`
}

// Judgment belongs to the parent. Problem correlates claims, never assigns correctness.
// Grade/Security are the parent's canonical severity, not the reported severity.
type Judgment struct {
	RunID        string       `json:"runId"`
	FindingID    string       `json:"findingId"`
	Parent       string       `json:"parent"`
	Problem      string       `json:"problem"`
	Verdict      Verdict      `json:"verdict"`
	Grade        review.Grade `json:"grade"`
	Security     bool         `json:"security"`
	CodeChanged  *bool        `json:"codeChanged"`
	ChangeCommit string       `json:"changeCommit"`
	Note         string       `json:"note"`
}

// Record has exactly one payload; later judgments supersede earlier judgments in file order.
type Record struct {
	Schema   string    `json:"schema"`
	ID       string    `json:"id"`
	Run      *Run      `json:"run,omitempty"`
	Judgment *Judgment `json:"judgment,omitempty"`
}
