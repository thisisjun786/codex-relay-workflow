// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/domain/finding.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package review

// Grade is a finding's severity, P0 (most severe) to P3; empty means the reported severity was not recognised.
type Grade string

// The four grades. Security is a flag on the finding, not a grade.
const (
	P0 Grade = "P0"
	P1 Grade = "P1"
	P2 Grade = "P2"
	P3 Grade = "P3"
)

// Verdict is the verification pass's judgement. VerdictUnverified also stands for a pass that did not run or failed.
type Verdict string

// The four verdicts.
const (
	VerdictConfirmed  Verdict = "confirmed"
	VerdictRejected   Verdict = "rejected"
	VerdictUncertain  Verdict = "uncertain"
	VerdictUnverified Verdict = "unverified"
)

// Finding is one problem after grouping. Reviewers are the reviewers that found it (ACR's Sources), strictly ascending, and Support
// is their count (ACR's ReviewerCount). Severity is the text the reviewer reported; Grade and Security come from NormalizeGrade.
// Everything a reviewer wrote is data: nothing in this package executes or follows it.
type Finding struct {
	File        string  `json:"file"`
	Line        int     `json:"line"`
	EndLine     int     `json:"endLine,omitempty"` // 0, or the last line of a range that starts at Line
	Title       string  `json:"title"`
	Explanation string  `json:"explanation"`
	Severity    string  `json:"severity"`
	Grade       Grade   `json:"grade"`
	Security    bool    `json:"security"`
	Perspective string  `json:"perspective"`
	Reviewers   []int   `json:"reviewers"`
	Support     int     `json:"support"`
	Verdict     Verdict `json:"verdict"`
}

// DropReason says why a finding was removed (the CRW form of ACR's Disposition kinds); Apply tries the rules in this order.
type DropReason string

// The nine reasons.
const (
	ReasonNonFinding      DropReason = "non_finding"
	ReasonNoiseFile       DropReason = "noise_file"
	ReasonInvalidLocation DropReason = "invalid_location"
	ReasonNotInDiff       DropReason = "not_in_diff"
	ReasonMissingAtHead   DropReason = "missing_at_head"
	ReasonLineBeyondHead  DropReason = "line_beyond_head"
	ReasonUnknownGrade    DropReason = "unknown_grade"
	ReasonRejected        DropReason = "rejected_by_verification"
	ReasonBelowThreshold  DropReason = "below_confidence_threshold"
)

// Drop is a finding a rule removed, as received (an unrecognised verdict is stored as unverified), with the reason and a short detail.
type Drop struct {
	Finding Finding    `json:"finding"`
	Reason  DropReason `json:"reason"`
	Detail  string     `json:"detail,omitempty"`
}

// Result is what Rules.Apply returns: the kept findings in input order, and the drops. Neither is nil.
type Result struct {
	Findings []Finding
	Dropped  []Drop
}

var (
	allGrades   = []Grade{P0, P1, P2, P3}
	allVerdicts = []Verdict{VerdictConfirmed, VerdictRejected, VerdictUncertain, VerdictUnverified}
	allReasons  = []DropReason{ReasonNonFinding, ReasonNoiseFile, ReasonInvalidLocation, ReasonNotInDiff, ReasonMissingAtHead,
		ReasonLineBeyondHead, ReasonUnknownGrade, ReasonRejected, ReasonBelowThreshold}
)
