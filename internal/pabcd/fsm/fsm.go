package fsm

import (
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Stub: every function answers a zero value, so the tests fail by assertion until the port lands.

func Order() []state.Phase                                  { return nil }
func ValidTransitions() map[state.Phase][]state.Phase       { return nil }
func IsLegalEdge(from, to state.Phase) bool                 { return false }
func CanEnter(to state.Phase, s state.State) (bool, string) { return true, "" }
func NextPhase(s state.State) (state.Phase, bool)           { return "", false }
func IsAuditGateOpen(s state.State) bool                    { return false }
func IsBuildGateOpen(s state.State) bool                    { return false }
func IsDone(s state.State) bool                             { return false }
func IsIdle(s state.State) bool                             { return false }
func DeriveInterviewFlag(s state.State) state.State         { return s }

// TransitionResult is the outcome of Transition.
type TransitionResult struct {
	OK     bool
	State  *state.State
	Reason string
}

func Transition(s state.State, to state.Phase, att *attest.Attestation) TransitionResult {
	return TransitionResult{}
}
