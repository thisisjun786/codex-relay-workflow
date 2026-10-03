// Package attest is a stub: the tests of this package were written first and fail against it.
package attest

import "github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"

const (
	VerdictPass     = "pass"
	VerdictNearPass = "near-pass"
	VerdictFail     = "fail"
)

type Attestation struct {
	From            state.Phase `json:"from"`
	To              state.Phase `json:"to"`
	Did             string      `json:"did"`
	AuditOutput     string      `json:"auditOutput,omitempty"`
	AuditVerdict    string      `json:"auditVerdict,omitempty"`
	AuditResidual   string      `json:"auditResidual,omitempty"`
	AuditRounds     *float64    `json:"auditRounds,omitempty"`
	CheckOutput     string      `json:"checkOutput,omitempty"`
	ExitCode        *float64    `json:"exitCode,omitempty"`
	Override        bool        `json:"override,omitempty"`
	PlanUnit        string      `json:"planUnit,omitempty"`
	PlanPaths       []string    `json:"planPaths,omitempty"`
	WorkPhaseID     string      `json:"workPhaseId,omitempty"`
	TestReceiptPath string      `json:"testReceiptPath,omitempty"`
}

type Result struct {
	OK      bool
	Reason  string
	Reasons []string
}

type PlanResult struct {
	OK     bool
	Unit   string
	Reason string
}

func IsGated(from, to state.Phase) bool                                { return false }
func GatedTransitions() []string                                       { return nil }
func IsAuditVerdict(v string) bool                                     { return false }
func Coerce(v any) *Attestation                                        { return nil }
func Validate(from, to state.Phase, att *Attestation) Result           { return Result{} }
func ValidateWorkPhaseBinding(att *Attestation, active *string) Result { return Result{} }
func HasFailVerdictTail(auditOutput string) bool                       { return false }
func ValidatePlanArtifacts(att *Attestation, cwd string) PlanResult    { return PlanResult{} }
