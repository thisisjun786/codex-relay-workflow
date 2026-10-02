package interview

// RED STUB: bodies return the zero answer so the ported tests fail by assertion.

// IsInterviewReady is the readiness predicate.
func IsInterviewReady(t *Tracker) bool { return false }

// Gate is the I->P soft-gate evaluation.
type Gate struct {
	Ready                  bool     `json:"ready"`
	ScanRan                bool     `json:"scanRan"`
	HighContradictionCount int      `json:"highContradictionCount"`
	Warnings               []string `json:"warnings"`
}

// GateEvidence is the ledger provenance the I->P decision passes.
type GateEvidence struct {
	BackedDimensions map[Dimension]bool
}

// EvaluateInterviewGate evaluates the I->P soft gate.
func EvaluateInterviewGate(t *Tracker, evidence *GateEvidence) Gate { return Gate{} }
