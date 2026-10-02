// Package interview ports the IPABCD interview tracker of CXC v0.2.40 (pabcd-state
// interview.ts, commit 3c1459ac): the tracker schema, the bounded fail-closed reconstruct, the
// readiness predicate (the one source of the session's interview flag) and the I->P soft gate.
// It is a pure library: it reads no file and starts no process.
//
// Behaviour is ported as-is. Malformed or lossy data reconstructs fail closed: an invalid level
// is "low", an invalid confidence 0, a legacy assumption unrecorded, an unknown severity
// "high", so corrupted data never becomes a ready tracker. Arrays are capped at
// MaxTrackerArray, dropping the oldest. Three representations are not literal:
//
//   - Counters are int64; the oracle floors a finite non-negative JavaScript number, and a
//     value at or above 2^63 saturates at math.MaxInt64 (no writer produces one).
//   - A nil slice is a missing array and fails readiness, as an absent or null array does in
//     the oracle. Constructors and Normalize return non-nil slices so a tracker marshals []
//     rather than null; marshal what Normalize returned (as the oracle's writeState does),
//     with HTML escaping off, because JSON.stringify does not escape <, > or &.
//   - Reconstruct* take what encoding/json decodes into an any, and accept json.Number, int
//     and int64 where a number is expected. A numeric string is not a number.
package interview

// Dimension is one of the four interview axes.
type Dimension string

// The interview axes, in DimensionOrder's order.
const (
	DimensionGoal       Dimension = "goal"
	DimensionConstraint Dimension = "constraint"
	DimensionSuccess    Dimension = "success"
	DimensionOntology   Dimension = "ontology"
)

// DimensionOrder is the oracle's DIMENSIONS, a function so the package holds no initialised
// variable.
func DimensionOrder() [4]Dimension {
	return [4]Dimension{DimensionGoal, DimensionConstraint, DimensionSuccess, DimensionOntology}
}

// DimensionLevel is how complete one dimension is.
type DimensionLevel string

// The levels, lowest first.
const (
	LevelLow  DimensionLevel = "low"
	LevelMid  DimensionLevel = "mid"
	LevelHigh DimensionLevel = "high"
	LevelMax  DimensionLevel = "max"
)

// ContradictionSeverity is how serious an open contradiction is.
type ContradictionSeverity string

// The severities, lowest first.
const (
	SeverityLow    ContradictionSeverity = "low"
	SeverityMedium ContradictionSeverity = "medium"
	SeverityHigh   ContradictionSeverity = "high"
)

const (
	// MaxTrackerArray caps every tracker array, dropping the oldest.
	MaxTrackerArray = 50
	// MaxAutoRounds is the most auto-resolve rounds one interview may spend.
	MaxAutoRounds = 5
)

// DimensionScore is the state of one dimension; Confidence is within [0, 1].
type DimensionScore struct {
	Level      DimensionLevel `json:"level"`
	Known      []string       `json:"known"`
	Unknown    []string       `json:"unknown"`
	Confidence float64        `json:"confidence"`
}

// Dimensions holds one score per axis, in the oracle's JSON key order.
type Dimensions struct {
	Goal       DimensionScore `json:"goal"`
	Constraint DimensionScore `json:"constraint"`
	Success    DimensionScore `json:"success"`
	Ontology   DimensionScore `json:"ontology"`
}

// Score returns the score of dim for reading or in-place update, nil for a non-axis name.
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

// Assumption is an assumption the interview made; readiness needs every one recorded.
// Severity is carried over from the originating contradiction and RequiresUserReview marks a
// recorded assumption the user still has to review.
type Assumption struct {
	ID                 string                `json:"id"`
	Text               string                `json:"text"`
	Recorded           bool                  `json:"recorded"`
	Severity           ContradictionSeverity `json:"severity,omitempty"`
	RequiresUserReview bool                  `json:"requiresUserReview,omitempty"`
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

// Tracker is the interview tracker persisted in the session state. RoundID is monotonic per
// interview; AutoResolveCount is capped by MaxAutoRounds and ConsecutiveAutoResolves resets
// when a contradiction escalates or resolves; ScanRounds counts the recorded contradiction
// scans and LastScanRoundID is the RoundID of the latest (0 = none). OntologySchema is absent
// unless non-empty.
type Tracker struct {
	RoundID                 int64            `json:"roundId"`
	Dimensions              Dimensions       `json:"dimensions"`
	Contradictions          []Contradiction  `json:"contradictions"`
	Assumptions             []Assumption     `json:"assumptions"`
	AutoResolveCount        int64            `json:"autoResolveCount"`
	ConsecutiveAutoResolves int64            `json:"consecutiveAutoResolves"`
	ScanRounds              int64            `json:"scanRounds"`
	LastScanRoundID         int64            `json:"lastScanRoundId"`
	OntologySchema          []OntologyEntity `json:"ontologySchema,omitempty"`
}

// DefaultScore is the fail-closed score of an unanswered dimension.
func DefaultScore() DimensionScore {
	return DimensionScore{Level: LevelLow, Known: []string{}, Unknown: []string{}}
}

// DefaultInterview is a fresh tracker; a negative roundID is 0.
func DefaultInterview(roundID int64) *Tracker {
	d := DefaultScore
	return &Tracker{
		RoundID:        roundIDNum(roundID),
		Dimensions:     Dimensions{Goal: d(), Constraint: d(), Success: d(), Ontology: d()},
		Contradictions: []Contradiction{},
		Assumptions:    []Assumption{},
	}
}
