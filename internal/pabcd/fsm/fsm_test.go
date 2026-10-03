package fsm

import (
	"reflect"
	"regexp"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The tests below are the 26 of fsm.test.ts in the oracle's order, plus TestStateCopyIsShallow.

const (
	idle, pI, pP, pA, pB, pC, pD = state.PhaseIdle, state.PhaseI, state.PhaseP, state.PhaseA, state.PhaseB, state.PhaseC, state.PhaseD
)

func withFlags(f state.Flags, phase state.Phase) state.State {
	s := state.DefaultState("t", "")
	s.Phase, s.Flags = phase, f
	return s
}

func can(to state.Phase, s state.State) bool { ok, _ := CanEnter(to, s); return ok }

func expect(t *testing.T, got bool, want bool, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func did(from, to state.Phase, text string) *attest.Attestation {
	return &attest.Attestation{From: from, To: to, Did: text}
}

func TestIToPBlockedWithoutInterviewFlag(t *testing.T) {
	expect(t, can(pP, withFlags(state.Flags{}, pI)), false, "I->P without interview")
	expect(t, can(pP, withFlags(state.Flags{Interview: true}, pI)), true, "I->P with interview")
}

func TestAEnterableFromPIllegalFromI(t *testing.T) {
	expect(t, can(pA, withFlags(state.Flags{}, pP)), true, "P->A")
	expect(t, can(pA, withFlags(state.Flags{}, pI)), false, "I->A")
}

func TestBBlockedWithoutAuditPassed(t *testing.T) {
	expect(t, can(pB, withFlags(state.Flags{}, pA)), false, "A->B closed")
	expect(t, can(pB, withFlags(state.Flags{AuditPassed: true}, pA)), true, "A->B open")
}

func TestDBlockedWithoutCheckPassed(t *testing.T) {
	expect(t, can(pD, withFlags(state.Flags{}, pC)), false, "C->D closed")
	expect(t, can(pD, withFlags(state.Flags{CheckPassed: true}, pC)), true, "C->D open")
}

func TestNextPhaseFollowsOrderAndClosesDToIdle(t *testing.T) {
	for _, c := range []struct{ from, want state.Phase }{{pI, pP}, {pC, pD}, {pD, idle}} {
		if got, ok := NextPhase(withFlags(state.Flags{}, c.from)); !ok || got != c.want {
			t.Errorf("NextPhase(%s) = %q, %v; want %q", c.from, got, ok, c.want)
		}
	}
}

func TestOrderIsTheCanonicalSequence(t *testing.T) {
	if got := Order(); !reflect.DeepEqual(got, []state.Phase{pI, pP, pA, pB, pC, pD}) {
		t.Errorf("Order() = %v", got)
	}
}

func TestGateHelpers(t *testing.T) {
	expect(t, IsAuditGateOpen(withFlags(state.Flags{}, pA)), true, "audit gate in A")
	expect(t, IsAuditGateOpen(withFlags(state.Flags{AuditPassed: true}, pB)), true, "audit gate with flag")
	expect(t, IsBuildGateOpen(withFlags(state.Flags{}, pI)), false, "build gate closed")
	expect(t, IsDone(withFlags(state.Flags{CheckPassed: true}, pD)), true, "done in D")
	expect(t, IsDone(withFlags(state.Flags{CheckPassed: true}, pC)), false, "not done in C")
}

func TestDefaultStateRestsAtIdle(t *testing.T) {
	s := state.DefaultState("t", "")
	if s.Phase != idle || !IsIdle(s) {
		t.Errorf("default phase %q, IsIdle %v", s.Phase, IsIdle(s))
	}
}

func TestIdleToPNeedsNoInterview(t *testing.T) {
	expect(t, can(pP, withFlags(state.Flags{}, idle)), true, "IDLE->P")
	if got, ok := NextPhase(withFlags(state.Flags{}, pD)); !ok || got != idle {
		t.Errorf("NextPhase(D) = %q, %v", got, ok)
	}
	if got, ok := NextPhase(withFlags(state.Flags{}, idle)); ok || got != "" {
		t.Errorf("NextPhase(IDLE) = %q, %v; want none", got, ok)
	}
}

func TestTransitionAToBRejectedWithoutAttestation(t *testing.T) {
	r := Transition(withFlags(state.Flags{}, pA), pB, nil)
	if r.OK || !regexp.MustCompile("(?i)attestation|did").MatchString(r.Reason) {
		t.Errorf("got %+v", r)
	}
}

func TestTransitionAToBFlipsAuditPassedOnlyWithAuditEvidence(t *testing.T) {
	a := did(pA, pB, "challenged the plan")
	if r := Transition(withFlags(state.Flags{}, pA), pB, a); r.OK || !regexp.MustCompile("auditOutput").MatchString(r.Reason) {
		t.Errorf("did alone: %+v", r)
	}
	a.AuditOutput = "reviewer verdict: GO; no blockers"
	if r := Transition(withFlags(state.Flags{}, pA), pB, a); r.OK || !regexp.MustCompile("auditVerdict").MatchString(r.Reason) {
		t.Errorf("no verdict: %+v", r)
	}
	a.AuditVerdict = attest.VerdictPass
	r := Transition(withFlags(state.Flags{}, pA), pB, a)
	if !r.OK || r.State.Phase != pB || !r.State.Flags.AuditPassed {
		t.Errorf("full attest: %+v", r)
	}
}

func TestTransitionCToDNeedsPassingCheckOutput(t *testing.T) {
	one, zero := 1.0, 0.0
	a := &attest.Attestation{From: pC, To: pD, Did: "ran tests", CheckOutput: "x", ExitCode: &one}
	if r := Transition(withFlags(state.Flags{}, pC), pD, a); r.OK {
		t.Errorf("exit 1: %+v", r)
	}
	a.CheckOutput, a.ExitCode = "77 pass", &zero
	if r := Transition(withFlags(state.Flags{}, pC), pD, a); !r.OK || !r.State.Flags.CheckPassed {
		t.Errorf("exit 0: %+v", r)
	}
}

func TestTransitionDToIdleClosesTheCycle(t *testing.T) {
	s := withFlags(state.Flags{AuditPassed: true, CheckPassed: true}, pD)
	phase := pC
	s.LastInjectedPhase, s.OrchestrationActive = &phase, true
	r := Transition(s, idle, nil)
	if !r.OK || r.State.Phase != idle || r.State.Flags != (state.Flags{}) || r.State.OrchestrationActive || r.State.LastInjectedPhase != nil {
		t.Errorf("got %+v", r)
	}
}

func readyTracker() *interview.Tracker {
	tr := interview.DefaultInterview(1)
	for _, d := range interview.DimensionOrder() {
		*tr.Dimensions.Score(d) = interview.DimensionScore{Level: interview.LevelMax, Known: []string{}, Unknown: []string{}, Confidence: 1}
	}
	tr.ScanRounds = 1 // readiness requires scan evidence
	return tr
}

func TestDeriveInterviewFlagFollowsTheTracker(t *testing.T) {
	base := state.DefaultState("t", "")
	base.Phase = pI
	none := DeriveInterviewFlag(base)
	expect(t, none.Flags.Interview, false, "no tracker")
	expect(t, can(pP, none), false, "P without tracker")
	base.Interview = readyTracker()
	ready := DeriveInterviewFlag(base)
	expect(t, ready.Flags.Interview, true, "ready tracker")
	expect(t, can(pP, ready), true, "P with ready tracker")
}

func TestValidTransitionsIsTheJawTableWithTheAToPEdge(t *testing.T) {
	want := map[state.Phase][]state.Phase{idle: {pI, pP}, pI: {pP, idle}, pP: {pI, pA}, pA: {pI, pB, pP}, pB: {pI, pC}, pC: {pI, pD, pB, pP}, pD: {pI, idle}}
	if got := ValidTransitions(); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v", got)
	}
}

func TestIsLegalEdge(t *testing.T) {
	for _, e := range [][2]state.Phase{{idle, pP}, {pP, pA}, {pA, pB}, {pB, pC}, {pC, pD}, {pD, idle}, {pC, pB}, {pC, pP}, {pA, pP}} {
		expect(t, IsLegalEdge(e[0], e[1]), true, string(e[0])+"->"+string(e[1]))
	}
	for _, e := range [][2]state.Phase{{idle, pA}, {idle, pB}, {pI, pA}, {pP, pC}, {pA, pD}, {pB, pD}} {
		expect(t, IsLegalEdge(e[0], e[1]), false, string(e[0])+"->"+string(e[1]))
	}
}

func TestAToPNeedsNoInterviewFlag(t *testing.T) {
	expect(t, can(pP, withFlags(state.Flags{}, pA)), true, "A->P")
}

func TestAToPOpensNoPathAroundTheAuditGate(t *testing.T) {
	replanned := Transition(withFlags(state.Flags{AuditPassed: true}, pA), pP, did(pA, pP, "three failed rounds; rewriting the plan"))
	if !replanned.OK || replanned.State.Phase != pP {
		t.Fatalf("A->P: %+v", replanned)
	}
	reaudit := Transition(*replanned.State, pA, did(pP, pA, "re-audit the changed plan"))
	if !reaudit.OK {
		t.Fatalf("P->A: %+v", reaudit)
	}
	if bare := Transition(*reaudit.State, pB, did(pA, pB, "trying to build")); bare.OK || !regexp.MustCompile("auditOutput").MatchString(bare.Reason) {
		t.Errorf("A->B without a fresh audit: %+v", bare)
	}
}

func TestAToPDoesNotMakeAToDLegal(t *testing.T) {
	expect(t, IsLegalEdge(pA, pD), false, "A->D edge")
	expect(t, can(pD, withFlags(state.Flags{CheckPassed: true}, pA)), false, "A->D entry")
}

func TestCanEnterNamesTheIllegalJump(t *testing.T) {
	if ok, reason := CanEnter(pA, withFlags(state.Flags{}, idle)); ok || reason != "illegal transition IDLE->A" {
		t.Errorf("got %v, %q", ok, reason)
	}
}

func TestPositiveFlagsDoNotBypassAdjacency(t *testing.T) {
	expect(t, can(pB, withFlags(state.Flags{AuditPassed: true}, pI)), false, "I->B with auditPassed")
	expect(t, can(pD, withFlags(state.Flags{CheckPassed: true}, pI)), false, "I->D with checkPassed")
}

func TestOnlyDAndICloseToIdle(t *testing.T) {
	expect(t, can(idle, withFlags(state.Flags{}, pD)), true, "D->IDLE")
	expect(t, can(idle, withFlags(state.Flags{}, pI)), true, "I->IDLE")
	for _, from := range []state.Phase{pP, pA, pB, pC} {
		expect(t, can(idle, withFlags(state.Flags{}, from)), false, string(from)+"->IDLE")
	}
}

func TestCToPReplansAndCToBStillNeedsAuditPassed(t *testing.T) {
	expect(t, can(pP, withFlags(state.Flags{}, pC)), true, "C->P")
	expect(t, can(pB, withFlags(state.Flags{}, pC)), false, "C->B without auditPassed")
	expect(t, can(pB, withFlags(state.Flags{AuditPassed: true}, pC)), true, "C->B with auditPassed")
}

func TestNextPhaseIsAlwaysALegalEdge(t *testing.T) {
	for _, from := range []state.Phase{pI, pP, pA, pB, pC, pD} {
		if to, ok := NextPhase(withFlags(state.Flags{}, from)); ok && !IsLegalEdge(from, to) {
			t.Errorf("NextPhase(%s) = %s is not a legal edge", from, to)
		}
	}
}

func TestTransitionBToCNeedsAnAttestation(t *testing.T) {
	if r := Transition(withFlags(state.Flags{AuditPassed: true}, pB), pC, nil); r.OK || !regexp.MustCompile("(?i)attestation|did").MatchString(r.Reason) {
		t.Errorf("no attest: %+v", r)
	}
	if r := Transition(withFlags(state.Flags{AuditPassed: true}, pB), pC, did(pB, pC, "implemented the audited plan")); !r.OK || r.State.Phase != pC {
		t.Errorf("with attest: %+v", r)
	}
}

func TestTransitionPToAIsGated(t *testing.T) {
	if r := Transition(withFlags(state.Flags{}, pP), pA, nil); r.OK || !regexp.MustCompile("(?i)attestation|did").MatchString(r.Reason) {
		t.Errorf("no attest: %+v", r)
	}
	if r := Transition(withFlags(state.Flags{}, pP), pA, did(pP, pA, "wrote the diff-level plan")); !r.OK || r.State.Phase != pA {
		t.Errorf("with attest: %+v", r)
	}
}

// A state is copied as a JavaScript spread copies it: the references inside it are shared, not cloned.
func TestStateCopyIsShallow(t *testing.T) {
	s := withFlags(state.Flags{}, idle)
	s.Interview, s.InjectedTurns = readyTracker(), []string{"x"}
	if d := DeriveInterviewFlag(s); d.Interview != s.Interview || &d.InjectedTurns[0] != &s.InjectedTurns[0] {
		t.Error("DeriveInterviewFlag cloned a reference")
	}
	r := Transition(s, pP, nil)
	if !r.OK || r.State.Interview != s.Interview || &r.State.InjectedTurns[0] != &s.InjectedTurns[0] {
		t.Errorf("Transition cloned a reference: %+v", r)
	}
}
