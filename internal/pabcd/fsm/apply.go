package fsm

import (
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The human free-pass path of the chat commands: the Go form of CXC v0.2.40 pabcd-state/src/orchestrate-apply.ts (whole file, commit
// 3c1459ac). A command that arrives from the chat advances a forward edge without an attestation: the table of legal edges still
// holds and only the evidence requirement is waived. The agent path keeps Transition and its attestation gate, which this file never
// calls. It is pure, as the oracle's is: it returns the next state and the ledger row for the caller to persist, and writes nothing.
//
// Behaviour is ported as-is, oracle defects included (docs/port-cxc/known-defects.md). What no caller can see differs in four ways:
//
//   - ApplyHumanTransition reads the clock once, for the row's ts; applyHumanTransition takes the instant, so a test can pin it.
//   - The I>P override and the generic path ask CanEnter the same question (the same phase and flags), so the override builds its row
//     where the oracle returns it early: the answer is the same.
//   - The oracle says it never throws, but its canEnter throws a TypeError for a phase that is a key of Object.prototype. As in
//     fsm.go the port answers a refusal, and a state read by ReadStateStrict never holds such a phase.
//   - The oracle tests checkEpoch and dcloseRecovery against null; nil here covers null and undefined alike.
//
// scanEvidence.scanRounds is the tracker's own count (state.interview?.scanRounds ?? 0), where highContradictionCount is the gate's.

// Control is what a chat command asks for beyond a forward move.
type Control string

// The controls of ApplyResult: none for a plain move, a status, a reset, and a D that closes the cycle to IDLE.
const (
	ControlNone   Control = ""
	ControlStatus Control = "status"
	ControlReset  Control = "reset"
	ControlDone   Control = "done"
)

// ApplyResult is ApplyResult. State is the next state to persist, nil on a refusal, a status and the reset no-op; Ledger is the row to
// append on a state change; Reason is the refusal; Noop marks a recognised no-op (reset from rest).
type ApplyResult struct {
	OK      bool
	State   *state.State
	Reason  string
	Ledger  *state.LedgerEntry
	Control Control
	Noop    bool
}

// timestampLayout is Date.prototype.toISOString, as package state writes a row's time (its own constant is not exported).
const timestampLayout = "2006-01-02T15:04:05.000Z"

// objectSource is the function Object as a JavaScript template literal prints it. The oracle's grammar answers the verb "constructor"
// (VerbConstructor) with that function, and its refusal, "illegal transition <from>-><verb>", quotes it (known-defects.md).
const objectSource = "function Object() { [native code] }"

// ClearedIdle is clearedIdle: the resting state a reset or a close produces. The phase is IDLE, the gate flags are false, and the
// source snapshot, the plan and check bindings and the D-close marker are gone, so none outlives its cycle. loopArmSeen and every
// other field stay as they were (only an explicit reset clears loopArmSeen). The input is not changed.
func ClearedIdle(s state.State) state.State {
	s.Phase, s.Flags, s.OrchestrationActive, s.LastInjectedPhase, s.IdleEditNudges = state.PhaseIdle, state.Flags{}, false, nil, 0
	s.PhaseEntrySource, s.PlanUnit, s.PlanEpoch, s.CheckEpoch, s.DcloseRecovery = nil, nil, nil, nil, nil
	return s
}

// unlockedFlag is unlockedFlag: the gate flag of f that the forward edge from>to unlocks, auditPassed for A>B and checkPassed for
// C>D, or nil. ApplyHumanTransition never reaches the C>D arm: a D returns before it asks.
func unlockedFlag(f *state.Flags, from, to state.Phase) *bool {
	switch {
	case from == state.PhaseA && to == state.PhaseB:
		return &f.AuditPassed
	case from == state.PhaseC && to == state.PhaseD:
		return &f.CheckPassed
	}
	return nil
}

// withEvidence gives the row the attestation's did as its evidence when there is one: ...(attest?.did ? { evidence: attest.did } : {}).
func withEvidence(e *state.LedgerEntry, att *attest.Attestation) *state.LedgerEntry {
	if att != nil && att.Did != "" {
		did := att.Did
		e.Evidence = &did
	}
	return e
}

// ApplyHumanTransition is applyHumanTransition: it applies a chat orchestrate command to s. A status reads and a reset clears
// (nothing to do from rest with no leftover check epoch or D-close marker); a D from C closes the cycle to IDLE; any other verb
// is a move along a legal edge, with the gate flag it unlocks set. An I>P move needs a ready interview or an attestation with
// override, and then records who overrode what. Everything else is refused with a reason and no state.
func ApplyHumanTransition(s state.State, verb OrchestrateVerb, att *attest.Attestation) ApplyResult {
	return applyHumanTransition(s, verb, att, time.Now())
}

func applyHumanTransition(s state.State, verb OrchestrateVerb, att *attest.Attestation, now time.Time) ApplyResult {
	from, to, ts := s.Phase, state.Phase(verb), now.UTC().Format(timestampLayout)
	switch {
	case verb == VerbStatus:
		return ApplyResult{OK: true, Control: ControlStatus}
	case verb == VerbReset:
		// An explicit control override: it skips the table (which refuses a mid-cycle move to IDLE) and is the operator's stand-down,
		// so it clears loopArmSeen too. IDLE alone is not rest: a leftover marker or check epoch is work to clear.
		if from == state.PhaseIdle && s.CheckEpoch == nil && s.DcloseRecovery == nil {
			return ApplyResult{OK: true, Control: ControlReset, Noop: true}
		}
		next := ClearedIdle(s)
		next.LoopArmSeen = false
		return ApplyResult{OK: true, Control: ControlReset, State: &next, Ledger: &state.LedgerEntry{TS: ts, SessionID: s.SessionID, From: &from, To: state.PhaseIdle, Reason: "reset"}}
	case to == state.PhaseD:
		// The human D means "I am done": C>D and D>IDLE together, so D is never a resting phase.
		if from != state.PhaseC {
			return ApplyResult{Reason: "illegal transition " + string(from) + "->D"}
		}
		next, c := ClearedIdle(s), state.PhaseC
		return ApplyResult{OK: true, Control: ControlDone, State: &next, Ledger: withEvidence(&state.LedgerEntry{TS: ts, SessionID: s.SessionID, From: &c, To: state.PhaseIdle, Reason: "done"}, att)}
	}

	// The free pass: set the gate flag this forward edge unlocks, so the flag test in CanEnter opens without an attestation.
	next, overridden, scan := s, false, state.ScanEvidence{}
	if flag := unlockedFlag(&next.Flags, from, to); flag != nil {
		*flag = true
	}
	// I>P is the interview soft gate. CanEnter refuses it while the interview flag is down, and that flag is derived from the
	// tracker, so the decision is made here, before CanEnter: a ready interview opens it, an override opens it and is recorded, and
	// anything else is advised against. Shape only: this path has no cwd to read the interview ledger from.
	if from == state.PhaseI && to == state.PhaseP {
		switch gate := interview.EvaluateInterviewGate(s.Interview, nil); {
		case gate.Ready:
			next.Flags.Interview = true
		case att != nil && att.Override:
			next.Flags.Interview, overridden, scan.HighContradictionCount = true, true, float64(gate.HighContradictionCount)
			if s.Interview != nil {
				scan.ScanRounds = float64(s.Interview.ScanRounds)
			}
		default:
			return ApplyResult{Reason: "interview soft-gate: " + strings.Join(gate.Warnings, "; ") + ". Re-run a contradiction scan, or pass override:true to proceed anyway."}
		}
	}
	// Entering I starts a fresh cycle, so a stale flag a human set cannot leak across cycles.
	if to == state.PhaseI {
		next.Flags = state.Flags{}
	}
	if ok, reason := CanEnter(to, next); !ok {
		if verb == VerbConstructor {
			reason = "illegal transition " + string(from) + "->" + objectSource
		}
		return ApplyResult{Reason: reason}
	}
	next.Phase = to
	entry := withEvidence(&state.LedgerEntry{TS: ts, SessionID: s.SessionID, From: &from, To: to, Reason: "chat", Actor: "human"}, att)
	if overridden {
		yes := true
		entry.Override, entry.ScanEvidence = &yes, &scan
	}
	return ApplyResult{OK: true, State: &next, Ledger: entry}
}
