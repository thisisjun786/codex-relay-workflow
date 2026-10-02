// Package interview is the Go port of the IPABCD interview tracker of CXC v0.2.40
// (pabcd-state/src/interview.ts, commit 3c1459ac). The tracker records how complete the
// Interview phase is across four dimensions; IsInterviewReady is the single predicate from
// which the session's interview flag is derived, and EvaluateInterviewGate is the I->P soft
// gate. The package is a pure library: it reads no file, starts no process and registers
// nothing, so linking it changes no running session.
//
// Behaviour is ported as-is. Malformed or lossy data reconstructs fail closed: an invalid
// dimension level becomes "low", an invalid confidence 0, a legacy assumption unrecorded
// and an unknown contradiction severity "high", so corrupted data never becomes a ready
// tracker. Every in-state array is capped at MaxTrackerArray, dropping the oldest.
//
// Three representation rules follow from Go's types and are the only places the port is
// not literal:
//
//   - Counters are int64. The oracle floors a finite non-negative JavaScript number; a value
//     at or above 2^63 saturates at math.MaxInt64 here, and no writer can produce one.
//   - A nil slice is a missing array and fails the readiness predicate, as an absent or null
//     array does in the oracle. Every constructor and Normalize return non-nil slices so a
//     tracker marshals to [] and never to null; callers marshal only what Normalize returned,
//     as the oracle's writeState does.
//   - Reconstruct* take the values encoding/json decodes into an any (map[string]any,
//     []any, string, float64, bool, nil), and also json.Number, int and int64 where a
//     number is expected. A numeric string is not a number.
package interview

// Dimension is one of the four interview axes.
type Dimension string

// The four interview axes, in the order DimensionOrder returns them.
const (
	DimensionGoal       Dimension = "goal"
	DimensionConstraint Dimension = "constraint"
	DimensionSuccess    Dimension = "success"
	DimensionOntology   Dimension = "ontology"
)

// DimensionOrder returns the interview axes in the oracle's DIMENSIONS order. It is a
// function so the package holds no initialised variable.
func DimensionOrder() [4]Dimension {
	return [4]Dimension{DimensionGoal, DimensionConstraint, DimensionSuccess, DimensionOntology}
}

// DimensionLevel is how complete one dimension is.
type DimensionLevel string

// The levels of a dimension, lowest first.
const (
	LevelLow  DimensionLevel = "low"
	LevelMid  DimensionLevel = "mid"
	LevelHigh DimensionLevel = "high"
	LevelMax  DimensionLevel = "max"
)

// DimensionLevels returns the valid levels in the oracle's DIMENSION_LEVELS order.
func DimensionLevels() [4]DimensionLevel {
	return [4]DimensionLevel{LevelLow, LevelMid, LevelHigh, LevelMax}
}

// ContradictionSeverity is how serious an open contradiction is.
type ContradictionSeverity string

// The severities of a contradiction, lowest first.
const (
	SeverityLow    ContradictionSeverity = "low"
	SeverityMedium ContradictionSeverity = "medium"
	SeverityHigh   ContradictionSeverity = "high"
)

// ContradictionSeverities returns the valid severities in the oracle's
// CONTRADICTION_SEVERITIES order.
func ContradictionSeverities() [3]ContradictionSeverity {
	return [3]ContradictionSeverity{SeverityLow, SeverityMedium, SeverityHigh}
}

const (
	// MaxTrackerArray caps every tracker array, dropping the oldest, so the hot session JSON
	// stays small.
	MaxTrackerArray = 50
	// MaxAutoRounds is the most auto-resolve rounds one interview may spend before it must be
	// closed or escalated.
	MaxAutoRounds = 5
)

// DimensionScore is the state of one dimension. Confidence is within [0, 1].
type DimensionScore struct {
	Level      DimensionLevel `json:"level"`
	Known      []string       `json:"known"`
	Unknown    []string       `json:"unknown"`
	Confidence float64        `json:"confidence"`
}

// Dimensions holds one score per axis. The fields are in the oracle's JSON key order.
type Dimensions struct {
	Goal       DimensionScore `json:"goal"`
	Constraint DimensionScore `json:"constraint"`
	Success    DimensionScore `json:"success"`
	Ontology   DimensionScore `json:"ontology"`
}

// Score returns the score of dim for reading or in-place update, or nil for a name that is
// not one of the four axes.
func (d *Dimensions) Score(dim Dimension) *DimensionScore {
	switch dim {
	case DimensionGoal:
		return &d.Goal
	case DimensionConstraint:
		return &d.Constraint
	case DimensionSuccess:
		return &d.Success
	case DimensionOntology:
		return &d.Ontology
	}
	return nil
}

// Contradiction is an open contradiction; ContradictionID correlates a Mind finding.
type Contradiction struct {
	ContradictionID string                `json:"contradictionId"`
	Severity        ContradictionSeverity `json:"severity"`
	Summary         string                `json:"summary"`
}

// Assumption is an assumption the interview made. Readiness needs every one recorded.
type Assumption struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Recorded bool   `json:"recorded"`
	// Severity is carried over from the originating contradiction; absent when not valid.
	Severity ContradictionSeverity `json:"severity,omitempty"`
	// RequiresUserReview marks a recorded assumption that still needs the user's review.
	RequiresUserReview bool `json:"requiresUserReview,omitempty"`
}

// OntologyRelationship is a link from an ontology entity to another.
type OntologyRelationship struct {
	To   string `json:"to"`
	Kind string `json:"kind"`
}

// OntologyEntity is one entity of the optional structured seed ontology.
type OntologyEntity struct {
	Name          string                 `json:"name"`
	Fields        []string               `json:"fields"`
	Relationships []OntologyRelationship `json:"relationships"`
}

// Tracker is the interview tracker persisted in the session state.
type Tracker struct {
	// RoundID is monotonic per interview.
	RoundID    int64      `json:"roundId"`
	Dimensions Dimensions `json:"dimensions"`
	// Contradictions and Assumptions are capped at MaxTrackerArray.
	Contradictions []Contradiction `json:"contradictions"`
	Assumptions    []Assumption    `json:"assumptions"`
	// AutoResolveCount is the auto-resolve rounds spent this interview (cap MaxAutoRounds).
	AutoResolveCount int64 `json:"autoResolveCount"`
	// ConsecutiveAutoResolves resets when a contradiction escalates to the user or resolves.
	ConsecutiveAutoResolves int64 `json:"consecutiveAutoResolves"`
	// ScanRounds counts the contradiction scans recorded this interview.
	ScanRounds int64 `json:"scanRounds"`
	// LastScanRoundID is the RoundID at the most recent recorded scan (0 = none yet).
	LastScanRoundID int64 `json:"lastScanRoundId"`
	// OntologySchema is the optional structured seed ontology; absent unless non-empty.
	OntologySchema []OntologyEntity `json:"ontologySchema,omitempty"`
}

// DefaultScore is the fail-closed score of an unanswered dimension.
func DefaultScore() DimensionScore {
	return DimensionScore{Level: LevelLow, Known: []string{}, Unknown: []string{}}
}

// DefaultInterview is a fresh tracker: every dimension low, nothing recorded. A negative
// roundID is 0.
func DefaultInterview(roundID int64) *Tracker {
	return &Tracker{
		RoundID: roundIDNum(roundID),
		Dimensions: Dimensions{
			Goal:       DefaultScore(),
			Constraint: DefaultScore(),
			Success:    DefaultScore(),
			Ontology:   DefaultScore(),
		},
		Contradictions: []Contradiction{},
		Assumptions:    []Assumption{},
	}
}
