package review

// MinUncertainSupport is the number of reviewers that must have found a finding the verification pass was uncertain about for it
// to be kept without being severe.
const MinUncertainSupport = 2

// Decision is the outcome of the confidence threshold: Keep, or the Reason a finding is dropped.
type Decision struct {
	Keep   bool
	Reason DropReason
}

// isSevere reports whether a finding is P0, P1 or flagged security: the findings the threshold keeps even when uncertain.
func isSevere(f Finding) bool { return f.Security || f.Grade == P0 || f.Grade == P1 }

// Decide is the confidence threshold. severe is true for a P0 or P1 finding or one flagged security. The table is in the package
// documentation; verification that did not run or failed (unverified, or any unrecognised verdict) keeps the finding (fail-open).
func Decide(v Verdict, support int, severe bool) Decision {
	switch v {
	case VerdictConfirmed:
		return Decision{Keep: true}
	case VerdictRejected:
		return Decision{Reason: ReasonRejected}
	case VerdictUncertain:
		if support >= MinUncertainSupport || severe {
			return Decision{Keep: true}
		}
		return Decision{Reason: ReasonBelowThreshold}
	}
	return Decision{Keep: true}
}
