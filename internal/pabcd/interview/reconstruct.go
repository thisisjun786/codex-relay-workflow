package interview

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
)

// tail copies the last MaxTrackerArray elements of s (drop-oldest); the result is never nil.
func tail[T any](s []T) []T {
	if len(s) > MaxTrackerArray {
		s = s[len(s)-MaxTrackerArray:]
	}
	return append([]T{}, s...)
}

// number reads v as the oracle's typeof v === "number". A number too large for a float64
// reads as an infinity, which every caller rejects.
func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := strconv.ParseFloat(string(n), 64)
		return f, err == nil || errors.Is(err, strconv.ErrRange)
	}
	return 0, false
}

// roundIDNum is the oracle's roundIdNum: a finite non-negative number floored, else 0.
func roundIDNum(v any) int64 {
	switch n := v.(type) {
	case int:
		return max(int64(n), 0)
	case int64:
		return max(n, 0)
	}
	f, ok := number(v)
	if !ok || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	if f = math.Floor(f); f >= math.MaxInt64 {
		return math.MaxInt64
	}
	return int64(f)
}

// confidence is the oracle's fail-closed confidence: anything but a finite number in [0, 1]
// is 0 (never clamped), and a negative zero is 0.
func confidence(v any) float64 {
	f, ok := number(v)
	if !ok || !(f >= 0 && f <= 1) || f == 0 {
		return 0
	}
	return f
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

// strArray keeps the strings of an array, the last MaxTrackerArray; any other value is empty.
func strArray(v any) []string {
	out := []string{}
	arr, _ := v.([]any)
	for _, x := range arr {
		if s, ok := x.(string); ok {
			out = append(out, s)
		}
	}
	return tail(out)
}

// levelOf is the fail-closed level: anything but the four levels is "low".
func levelOf(s string) DimensionLevel {
	switch l := DimensionLevel(s); l {
	case LevelLow, LevelMid, LevelHigh, LevelMax:
		return l
	}
	return LevelLow
}

// severityOf is the fail-closed severity: an unknown one is "high" (ok false) so it blocks.
func severityOf(s string) (sev ContradictionSeverity, ok bool) {
	switch sev = ContradictionSeverity(s); sev {
	case SeverityLow, SeverityMedium, SeverityHigh:
		return sev, true
	}
	return SeverityHigh, false
}

func reconstructScore(v any) DimensionScore {
	m, ok := v.(map[string]any)
	if !ok {
		return DefaultScore()
	}
	return DimensionScore{Level: levelOf(str(m["level"])), Known: strArray(m["known"]), Unknown: strArray(m["unknown"]), Confidence: confidence(m["confidence"])}
}

// ReconstructInterview rebuilds a persisted tracker from a decoded JSON value, strictly,
// bounded and fail-closed; nil for anything but an object (a fresh session reads null). A
// non-object contradiction stays as a high-severity sentinel and a non-object assumption as
// an unrecorded one, so corrupt entries keep blocking readiness instead of being dropped.
func ReconstructInterview(v any) *Tracker {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	t := &Tracker{
		RoundID:                 roundIDNum(m["roundId"]),
		Contradictions:          []Contradiction{},
		Assumptions:             []Assumption{},
		AutoResolveCount:        roundIDNum(m["autoResolveCount"]),
		ConsecutiveAutoResolves: roundIDNum(m["consecutiveAutoResolves"]),
		ScanRounds:              roundIDNum(m["scanRounds"]),
		LastScanRoundID:         roundIDNum(m["lastScanRoundId"]),
		OntologySchema:          ReconstructOntologySchema(m["ontologySchema"]),
	}
	dims, _ := m["dimensions"].(map[string]any)
	for _, d := range DimensionOrder() {
		*t.Dimensions.Score(d) = reconstructScore(dims[string(d)])
	}
	contradictions, _ := m["contradictions"].([]any)
	for _, e := range contradictions {
		c := Contradiction{Severity: SeverityHigh, Summary: "[malformed contradiction entry]"}
		if r, ok := e.(map[string]any); ok {
			id, isString := r["contradictionId"].(string)
			if !isString {
				id = str(r["id"]) // a legacy key
			}
			c.ContradictionID, c.Summary = id, str(r["summary"])
			c.Severity, _ = severityOf(str(r["severity"]))
		}
		t.Contradictions = append(t.Contradictions, c)
	}
	t.Contradictions = tail(t.Contradictions)
	assumptions, _ := m["assumptions"].([]any)
	for _, e := range assumptions {
		a := Assumption{Text: "[malformed assumption entry]"}
		if r, ok := e.(map[string]any); ok {
			a = Assumption{ID: str(r["id"]), Text: str(r["text"])}
			a.Recorded, _ = r["recorded"].(bool)
			a.RequiresUserReview, _ = r["requiresUserReview"].(bool)
			if sev, ok := severityOf(str(r["severity"])); ok {
				a.Severity = sev
			}
		}
		t.Assumptions = append(t.Assumptions, a)
	}
	t.Assumptions = tail(t.Assumptions)
	return t
}

// ReconstructOntologySchema parses a decoded value into entities, dropping entries that are
// not objects or have no name and relationships with no target; nil when nothing survives.
func ReconstructOntologySchema(v any) []OntologyEntity {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	var entities []OntologyEntity
	for _, e := range arr {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		entity := OntologyEntity{Name: str(m["name"]), Fields: strArray(m["fields"])}
		rels, _ := m["relationships"].([]any)
		for _, r := range rels {
			if rm, ok := r.(map[string]any); ok {
				entity.Relationships = append(entity.Relationships, OntologyRelationship{To: str(rm["to"]), Kind: str(rm["kind"])})
			}
		}
		entities = append(entities, entity)
	}
	return cleanOntology(entities)
}

// cleanOntology applies the entity rules shared by reconstruct and Normalize: nameless
// entities and targetless relationships go, every list is capped, and nothing left is nil.
func cleanOntology(in []OntologyEntity) []OntologyEntity {
	out := []OntologyEntity{}
	for _, e := range in {
		if e.Name == "" {
			continue
		}
		rels := []OntologyRelationship{}
		for _, r := range e.Relationships {
			if r.To != "" {
				rels = append(rels, r)
			}
		}
		out = append(out, OntologyEntity{Name: e.Name, Fields: tail(e.Fields), Relationships: tail(rels)})
	}
	if len(out) == 0 {
		return nil
	}
	return tail(out)
}

// Normalize is the write-side normalisation: every array is capped at MaxTrackerArray
// (drop-oldest), each score gets the fail-closed rules and no array is left nil. It returns a
// new tracker; nil stays nil. Like the oracle it does not re-validate contradiction or
// assumption entries.
func Normalize(t *Tracker) *Tracker {
	if t == nil {
		return nil
	}
	n := &Tracker{
		RoundID:                 roundIDNum(t.RoundID),
		Contradictions:          tail(t.Contradictions),
		Assumptions:             tail(t.Assumptions),
		AutoResolveCount:        roundIDNum(t.AutoResolveCount),
		ConsecutiveAutoResolves: roundIDNum(t.ConsecutiveAutoResolves),
		ScanRounds:              roundIDNum(t.ScanRounds),
		LastScanRoundID:         roundIDNum(t.LastScanRoundID),
		OntologySchema:          cleanOntology(t.OntologySchema),
	}
	for _, d := range DimensionOrder() {
		s := t.Dimensions.Score(d)
		*n.Dimensions.Score(d) = DimensionScore{Level: levelOf(string(s.Level)), Known: tail(s.Known), Unknown: tail(s.Unknown), Confidence: confidence(s.Confidence)}
	}
	return n
}
