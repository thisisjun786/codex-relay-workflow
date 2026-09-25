package store

// PathBinding is the tier at which an artifact read was bound to its declared path.
type PathBinding string

const (
	BestEffortDetection PathBinding = "best_effort_detection"
	LeaseEnforced       PathBinding = "lease_enforced"
)
