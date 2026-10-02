package interview

// RED STUB: bodies return the zero answer so the ported tests fail by assertion.

func roundIDNum(v any) int64 { return 0 }

func confidence(v any) float64 { return 0 }

// ReconstructInterview rebuilds a tracker from a persisted value; nil when v is not an object.
func ReconstructInterview(v any) *Tracker { return &Tracker{} }

// ReconstructOntologySchema parses an unknown value into entities; nil when none survive.
func ReconstructOntologySchema(v any) []OntologyEntity { return nil }

// Normalize caps every array before the tracker is persisted.
func Normalize(t *Tracker) *Tracker { return &Tracker{} }
