package interview

import (
	"fmt"
	"strings"
)

const (
	warnNoScan   = "no contradiction scan has been recorded for this interview"
	warnNotReady = "interview is not ready (dimensions/assumptions incomplete)"
	// warnUnbackedTail follows the unbacked dimension names; the command is the oracle's
	// "cxc scan record" under the CRW name table.
	warnUnbackedTail = " reached \"high\" without an answered question in the interview ledger \u2014 " +
		"ask and record one per dimension (`crw pabcd scan record --derive --map <questionId>=<dimension>`), " +
		"or assert the level deliberately"
)

// validScore is the strict shape check of a score's arrays and confidence; a NaN fails it.
func validScore(s DimensionScore) bool {
	return s.Known != nil && s.Unknown != nil && s.Confidence >= 0 && s.Confidence <= 1
}

// IsInterviewReady is the readiness predicate, the single source of the interview flag. It
// is true only when t is non-nil, every dimension is a valid score at "high" or "max",
// contradictions is present and empty, every assumption is recorded and at least one
// contradiction scan was recorded. Nothing on the tracker can assert readiness; it is always
// recomputed, and a nil slice is a missing array.
func IsInterviewReady(t *Tracker) bool {
	if t == nil {
		return false
	}
	for _, d := range DimensionOrder() {
		s := *t.Dimensions.Score(d)
		if !validScore(s) || (s.Level != LevelHigh && s.Level != LevelMax) {
			return false
		}
	}
	if t.Contradictions == nil || len(t.Contradictions) > 0 || t.Assumptions == nil {
		return false
	}
	for _, a := range t.Assumptions {
		if !a.Recorded {
			return false
		}
	}
	return roundIDNum(t.ScanRounds) >= 1
}

// Gate is the pure, advisory I->P soft-gate evaluation: the caller may advise-block or, on an
// explicit override, proceed and log it. Warnings is empty when Ready.
type Gate struct {
	Ready                  bool     `json:"ready"`
	ScanRan                bool     `json:"scanRan"`
	HighContradictionCount int      `json:"highContradictionCount"`
	Warnings               []string `json:"warnings"`
}

// GateEvidence is what the interview ledger shows: the dimensions whose level traces to an
// answered question. Passing it, even empty, turns the provenance check on.
type GateEvidence struct {
	BackedDimensions map[Dimension]bool
}

// EvaluateInterviewGate is the I->P gate: data shape and provenance. A nil evidence is the
// shape-only decision of the human free-pass path. With evidence a "high" dimension must be
// backed by an answered question, while "max" is an explicit assertion that needs no backing;
// provenance is only checked once the shape is ready.
func EvaluateInterviewGate(t *Tracker, evidence *GateEvidence) Gate {
	g := Gate{ScanRan: t != nil && roundIDNum(t.ScanRounds) >= 1, Warnings: []string{}}
	if t != nil {
		for _, c := range t.Contradictions {
			if c.Severity == SeverityHigh {
				g.HighContradictionCount++
			}
		}
	}
	if !g.ScanRan {
		g.Warnings = append(g.Warnings, warnNoScan)
	}
	if g.HighContradictionCount > 0 {
		g.Warnings = append(g.Warnings, fmt.Sprintf("%d high-severity contradiction(s) still open", g.HighContradictionCount))
	}
	shapeReady := IsInterviewReady(t)
	var unbacked []string
	if shapeReady && evidence != nil {
		for _, d := range DimensionOrder() {
			if t.Dimensions.Score(d).Level == LevelHigh && !evidence.BackedDimensions[d] {
				unbacked = append(unbacked, string(d))
			}
		}
	}
	if len(unbacked) > 0 {
		g.Warnings = append(g.Warnings, strings.Join(unbacked, ", ")+warnUnbackedTail)
	}
	g.Ready = shapeReady && len(unbacked) == 0
	if !g.Ready && len(g.Warnings) == 0 {
		g.Warnings = append(g.Warnings, warnNotReady)
	}
	return g
}
