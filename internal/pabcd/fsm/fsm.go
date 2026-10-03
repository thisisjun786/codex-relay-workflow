// Package fsm is the legal-transition table, the gate flags and the chat command grammar of the PABCD loop: the Go form of CXC
// v0.2.40 pabcd-state/src/fsm.ts and orchestrate-grammar.ts (both whole files, commit 3c1459ac). It is a pure library: it reads no
// file, starts no process and holds no package-level state. It sits beside the state, attest and interview packages it uses, not
// in package state, which imports nothing back (attest already imports state).
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md). Order, ValidTransitions, IsLegalEdge,
// CanEnter, NextPhase, the four gate predicates, DeriveInterviewFlag and Transition are fsm.ts; OrchestrateCommand and
// ParseOrchestrateCommand are orchestrate-grammar.ts. A state is passed and returned by value, which copies it as a JavaScript
// spread does: the tracker, the slices and the pointer fields stay shared with the input, and nothing is cloned.
//
// The chat prefixes are crw's (name-substitution R1, R10, R27): "$crw:crw-", "$crw-", "crw" and white space, or "/". The oracle's
// own spellings are not kept as aliases. The JavaScript regular expressions of PREFIX and COMMAND are written out by hand,
// not with package regexp, so that \s, the dot, trim and the ASCII-only case folding of a non-unicode /i are JavaScript's.
//
// Not carried, each an intentional limit of the port and not a choice about the oracle's behaviour:
//
//   - An attest JSON nested deeper than 10000 levels is refused as not valid JSON: encoding/json (JSONv2-backed since Go 1.27, as
//     is its jsontext layer) stops at that depth where V8's JSON.parse does not.
//   - A thrown error is a refusal, as in package attest. The oracle throws a TypeError from IsLegalEdge, CanEnter and Transition
//     when the current phase is a key of Object.prototype, such as "constructor"; a Go caller can pass such a phase directly,
//     and gets the refusal "illegal transition". A state read by ReadStateStrict never holds one.
//   - The oracle's canEnter ends in an arm for an unknown phase that no input reaches, because the adjacency check refuses first.
//   - A lone surrogate escape in the attest JSON becomes U+FFFD (a Go string cannot hold one), as in package attest.
package fsm

import (
	"slices"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// Order is ORDER, the work phases I to D; IDLE is the rest state outside it.
func Order() []state.Phase { return state.WorkPhases() }

// ValidTransitions is VALID_TRANSITIONS: the legal edges of each phase, the cli-jaw table plus the A->P re-plan edge (LOOP-REPAIR-01).
// An edge outside it is illegal whatever the gate flags say. It is built per call, so the package holds no table.
func ValidTransitions() map[state.Phase][]state.Phase {
	idle, i, p, a, b, c, d := state.PhaseIdle, state.PhaseI, state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC, state.PhaseD
	return map[state.Phase][]state.Phase{idle: {i, p}, i: {p, idle}, p: {i, a}, a: {i, b, p}, b: {i, c}, c: {i, d, b, p}, d: {i, idle}}
}

// IsLegalEdge reports whether to follows from in the table; any other phase string has no edges.
func IsLegalEdge(from, to state.Phase) bool { return slices.Contains(ValidTransitions()[from], to) }

// CanEnter is canEnter: whether s may enter to, and the reason when it may not. An illegal edge is refused before any flag is
// read, so auditPassed or checkPassed never authorise a jump the table forbids. Closing to IDLE and entering I, A or C need no
// flag; P needs the interview flag except from IDLE, A or C (a re-plan is not a reason to redo discovery); B needs auditPassed
// and D needs checkPassed.
func CanEnter(to state.Phase, s state.State) (bool, string) {
	if !IsLegalEdge(s.Phase, to) {
		return false, "illegal transition " + string(s.Phase) + "->" + string(to)
	}
	switch from := s.Phase; to {
	case state.PhaseP:
		if from != state.PhaseIdle && from != state.PhaseC && from != state.PhaseA && !s.Flags.Interview {
			return false, "interview not completed (I->P needs interview flag)"
		}
	case state.PhaseB:
		if !s.Flags.AuditPassed {
			return false, "audit gate closed (need auditPassed via A->B attestation)"
		}
	case state.PhaseD:
		if !s.Flags.CheckPassed {
			return false, "check gate closed (need checkPassed via C->D attestation)"
		}
	}
	return true, ""
}

// NextPhase follows I, P, A, B, C, D and closes to IDLE after D. From IDLE the caller chooses the entry, so there is none.
func NextPhase(s state.State) (state.Phase, bool) {
	if s.Phase == state.PhaseD {
		return state.PhaseIdle, true
	}
	order := Order()
	if i := slices.Index(order, s.Phase); i >= 0 && i+1 < len(order) {
		return order[i+1], true
	}
	return "", false
}

// IsAuditGateOpen, IsBuildGateOpen, IsDone and IsIdle are the oracle's gate predicates.
func IsAuditGateOpen(s state.State) bool { return s.Phase == state.PhaseA || s.Flags.AuditPassed }
func IsBuildGateOpen(s state.State) bool { return s.Flags.AuditPassed }
func IsDone(s state.State) bool          { return s.Phase == state.PhaseD && s.Flags.CheckPassed }
func IsIdle(s state.State) bool          { return s.Phase == state.PhaseIdle }

// DeriveInterviewFlag returns s with the interview flag recomputed from its tracker, so a loose trigger cannot flip it.
func DeriveInterviewFlag(s state.State) state.State {
	s.Flags.Interview = interview.IsInterviewReady(s.Interview)
	return s
}

// TransitionResult is the outcome of Transition: the next state to persist, or the reason it was refused (State is nil then).
type TransitionResult struct {
	OK     bool
	State  *state.State
	Reason string
}

// Transition is transition: the one place the gate flags flip, and only for a gated forward edge (A>B, C>D) whose attestation
// Validate accepts. It validates the attestation, flips the flag the edge unlocks (and clears the interview flag on IDLE>I),
// asks CanEnter with the flipped flags, and on D>IDLE closes the cycle: gate flags, orchestrationActive and lastInjectedPhase are
// reset. It performs no IO, and the Reason of a refused attestation is attest's own text.
func Transition(s state.State, to state.Phase, att *attest.Attestation) TransitionResult {
	from := s.Phase
	if gate := attest.Validate(from, to, att); !gate.OK {
		return TransitionResult{Reason: gate.Reason}
	}
	switch {
	case from == state.PhaseA && to == state.PhaseB:
		s.Flags.AuditPassed = true
	case from == state.PhaseC && to == state.PhaseD:
		s.Flags.CheckPassed = true
	case from == state.PhaseIdle && to == state.PhaseI:
		s.Flags.Interview = false
	}
	if ok, reason := CanEnter(to, s); !ok {
		return TransitionResult{Reason: reason}
	}
	if from == state.PhaseD && to == state.PhaseIdle {
		s.Flags, s.OrchestrationActive, s.LastInjectedPhase = state.Flags{}, false, nil
	}
	s.Phase = to
	return TransitionResult{OK: true, State: &s}
}
