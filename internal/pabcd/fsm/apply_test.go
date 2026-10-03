package fsm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/attest"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/interview"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The first 14 tests are the 14 of orchestrate-apply.test.ts in the oracle's order. Then come the replay of the recorded oracle
// (testdata/oracle-apply.json, recorded by testdata/record-apply-oracle.mjs; no Node runs here), the helper test for the C>D arm
// that ApplyHumanTransition never reaches, and one test of the exported clock wrapper.

// frozen is the instant the recorder's clock stands at, so ledger rows compare byte for byte.
func frozen() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }

func apply(s state.State, verb OrchestrateVerb, att *attest.Attestation) ApplyResult {
	return applyHumanTransition(s, verb, att, frozen())
}

func at(phase state.Phase, f state.Flags) state.State { return withFlags(f, phase) }

func eq[T comparable](t *testing.T, got, want T, what string) {
	t.Helper()
	if got != want {
		t.Errorf("%s: got %v, want %v", what, got, want)
	}
}

func atI(tr *interview.Tracker) state.State {
	s := at(pI, state.Flags{})
	s.Interview = tr
	return s
}

func TestHumanFreePassAdvancesForwardEdgesWithoutAttest(t *testing.T) {
	for _, c := range []struct{ from, to state.Phase }{{pP, pA}, {pA, pB}, {pB, pC}} {
		r := apply(at(c.from, state.Flags{}), OrchestrateVerb(c.to), nil)
		if r.State == nil || r.State.Phase != c.to {
			t.Errorf("%s->%s: %+v", c.from, c.to, r)
		}
	}
	cd := apply(at(pC, state.Flags{}), VerbD, nil)
	eq(t, cd.Control, ControlDone, "C->D control")
	if cd.State == nil || cd.Ledger == nil {
		t.Fatalf("C->D: %+v", cd)
	}
	eq(t, cd.State.Phase, idle, "C->D phase")
	eq(t, cd.State.Flags.CheckPassed, false, "checkPassed cleared on close")
	eq(t, cd.Ledger.To, idle, "ledger to")
	eq(t, cd.Ledger.Reason, "done", "ledger reason")
}

func TestHumanDFromANonCPhaseIsRefused(t *testing.T) {
	eq(t, apply(at(pB, state.Flags{}), VerbD, nil).OK, false, "B->D")
}

func TestIllegalAdjacencyIsRefusedWithStateUnchangedAndNoLedger(t *testing.T) {
	r := apply(at(pP, state.Flags{}), VerbC, nil)
	eq(t, r.OK, false, "P->C ok")
	eq(t, strings.Contains(r.Reason, "illegal transition"), true, "reason "+r.Reason)
	if r.State != nil || r.Ledger != nil {
		t.Errorf("state or ledger present: %+v", r)
	}
}

func TestForwardMoveEmitsALedgerEntryWithReasonChat(t *testing.T) {
	r := apply(at(pP, state.Flags{}), VerbA, did(pP, pA, "wrote plan"))
	if r.Ledger == nil || r.Ledger.Evidence == nil {
		t.Fatalf("ledger: %+v", r)
	}
	eq(t, r.Ledger.To, pA, "ledger to")
	eq(t, r.Ledger.Reason, "chat", "ledger reason")
	eq(t, *r.Ledger.Evidence, "wrote plan", "ledger evidence")
}

func TestResetFromAMidCyclePhaseClearsFlagsAndReturnsIdle(t *testing.T) {
	r := apply(at(pC, state.Flags{AuditPassed: true, CheckPassed: true}), VerbReset, nil)
	eq(t, r.Control, ControlReset, "control")
	if r.State == nil || r.Ledger == nil {
		t.Fatalf("reset: %+v", r)
	}
	eq(t, r.State.Phase, idle, "phase")
	eq(t, r.State.Flags, state.Flags{}, "flags")
	eq(t, r.Ledger.Reason, "reset", "ledger reason")
}

func TestResetFromIdleIsARecognizedNoop(t *testing.T) {
	r := apply(at(idle, state.Flags{}), VerbReset, nil)
	eq(t, r.OK && r.Noop, true, "ok and noop")
	if r.State != nil || r.Ledger != nil {
		t.Errorf("state or ledger present: %+v", r)
	}
}

func TestStatusIsAReadOnlyNoop(t *testing.T) {
	r := apply(at(pB, state.Flags{AuditPassed: true}), VerbStatus, nil)
	eq(t, r.Control, ControlStatus, "control")
	if r.State != nil || r.Ledger != nil {
		t.Errorf("state or ledger present: %+v", r)
	}
}

func TestEnteringIClearsStaleGateFlags(t *testing.T) {
	r := apply(at(pD, state.Flags{AuditPassed: true, CheckPassed: true}), VerbI, nil)
	if r.State == nil {
		t.Fatalf("D->I: %+v", r)
	}
	eq(t, r.State.Phase, pI, "phase")
	eq(t, r.State.Flags, state.Flags{}, "flags")
}

func TestSoftGateReadyInterviewAdvancesIToP(t *testing.T) {
	r := apply(atI(readyTracker()), VerbP, nil)
	if !r.OK || r.State == nil {
		t.Fatalf("I->P: %+v", r)
	}
	eq(t, r.State.Phase, pP, "phase")
	eq(t, r.State.Flags.Interview, true, "interview flag")
}

func TestSoftGateNoScanRecordedAdviseBlocks(t *testing.T) {
	tr := readyTracker()
	tr.ScanRounds, tr.LastScanRoundID = 0, 0
	r := apply(atI(tr), VerbP, nil)
	eq(t, r.OK, false, "ok")
	eq(t, strings.Contains(r.Reason, "soft-gate") && strings.Contains(r.Reason, "scan"), true, "reason "+r.Reason)
}

func TestSoftGateHighContradictionOpenAdviseBlocks(t *testing.T) {
	tr := readyTracker()
	tr.Contradictions = []interview.Contradiction{{ContradictionID: "c1", Severity: interview.SeverityHigh, Summary: "x"}}
	r := apply(atI(tr), VerbP, nil)
	eq(t, r.OK, false, "ok")
	eq(t, strings.Contains(r.Reason, "high-severity"), true, "reason "+r.Reason)
}

func TestSoftGateExplicitOverrideAdvancesAndRecordsAnAuditLedgerEntry(t *testing.T) {
	tr := readyTracker()
	tr.ScanRounds = 0
	r := apply(atI(tr), VerbP, &attest.Attestation{From: pI, To: pP, Did: "accept risk, proceeding", Override: true})
	if !r.OK || r.State == nil || r.Ledger == nil || r.Ledger.Override == nil || r.Ledger.ScanEvidence == nil || r.Ledger.Evidence == nil {
		t.Fatalf("override: %+v", r)
	}
	eq(t, r.State.Phase, pP, "phase")
	eq(t, *r.Ledger.Override, true, "override")
	eq(t, r.Ledger.Actor, "human", "actor")
	eq(t, r.Ledger.ScanEvidence.ScanRounds, 0.0, "scanRounds")
	eq(t, *r.Ledger.Evidence, "accept risk, proceeding", "evidence")
}

func TestSoftGateOverrideIsGoalModeAgnostic(t *testing.T) {
	tr := readyTracker()
	tr.Contradictions = []interview.Contradiction{{ContradictionID: "c1", Severity: interview.SeverityHigh, Summary: "x"}}
	eq(t, apply(atI(tr), VerbP, nil).OK, false, "blocked")
	over := apply(atI(tr), VerbP, &attest.Attestation{From: pI, To: pP, Did: "go", Override: true})
	if !over.OK || over.Ledger == nil || over.Ledger.ScanEvidence == nil {
		t.Fatalf("override: %+v", over)
	}
	eq(t, over.Ledger.ScanEvidence.HighContradictionCount, 1.0, "highContradictionCount")
}

func TestResetFromIdleClearsADCloseMarkerAndCheckEpochInsteadOfBecomingANoop(t *testing.T) {
	s, epoch, next := at(idle, state.Flags{}), "c-recovery", "wp-2"
	s.CheckEpoch = &epoch
	s.DcloseRecovery = &state.DcloseRecoveryMarker{SessionID: "t", CheckEpoch: epoch, ClosedWorkPhaseID: "wp-1", NextWorkPhaseID: &next}
	r := apply(s, VerbReset, nil)
	eq(t, r.OK && !r.Noop, true, "ok and not noop")
	if r.State == nil || r.Ledger == nil {
		t.Fatalf("reset: %+v", r)
	}
	eq(t, r.State.CheckEpoch == nil && r.State.DcloseRecovery == nil, true, "epoch and marker cleared")
	eq(t, r.Ledger.Reason, "reset", "ledger reason")
}

func TestUnlockedFlag(t *testing.T) {
	for _, c := range []struct {
		from, to state.Phase
		want     func(*state.Flags) *bool
	}{
		{pA, pB, func(f *state.Flags) *bool { return &f.AuditPassed }},
		{pC, pD, func(f *state.Flags) *bool { return &f.CheckPassed }},
		{pP, pA, func(*state.Flags) *bool { return nil }},
		{pB, pC, func(*state.Flags) *bool { return nil }},
	} {
		var f state.Flags
		eq(t, unlockedFlag(&f, c.from, c.to), c.want(&f), string(c.from)+">"+string(c.to))
	}
}

func TestApplyHumanTransitionStampsTheLedgerWithTheCurrentTime(t *testing.T) {
	r := ApplyHumanTransition(at(pP, state.Flags{}), VerbA, nil)
	if r.Ledger == nil {
		t.Fatalf("P->A: %+v", r)
	}
	ts, err := time.Parse("2006-01-02T15:04:05.000Z", r.Ledger.TS)
	if err != nil || time.Since(ts).Abs() > time.Minute {
		t.Errorf("ts %q: %v", r.Ledger.TS, err)
	}
}

// diffKeys is the top-level keys that differ between before and after, each with its value in after (null when gone), as the recorder writes them.
func diffKeys(t *testing.T, before, after state.State) map[string]any {
	t.Helper()
	parse := func(s state.State) map[string]any {
		enc, err := state.Encode(s)
		must(t, err)
		var m map[string]any
		must(t, json.Unmarshal(enc, &m))
		return m
	}
	b, a, out := parse(before), parse(after), map[string]any{}
	for _, m := range []map[string]any{a, b} { // a key the call removed is recorded as null, as the recorder writes it
		for k := range m {
			if !reflect.DeepEqual(a[k], b[k]) {
				out[k] = a[k]
			}
		}
	}
	return out
}

func TestApplyMatchesTheRecordedOracle(t *testing.T) {
	var f struct {
		Seed                  json.RawMessage
		Trackers              map[string]json.RawMessage
		Attests               []*attest.Attestation
		Texts, Diffs, Ledgers []string
		Rows                  []row
	}
	load(t, "oracle-apply.json", &f)
	if len(f.Rows) < 1100 {
		t.Fatalf("%d recorded rows", len(f.Rows))
	}
	dir, wantLedger := t.TempDir(), []string{}
	for _, r := range f.Rows {
		var s state.State
		must(t, json.Unmarshal(f.Seed, &s))
		s.Phase, s.Flags = state.Phase(r.s(0)), state.Flags{Interview: r.n(2)&1 != 0, AuditPassed: r.n(2)&2 != 0, CheckPassed: r.n(2)&4 != 0}
		must(t, json.Unmarshal(f.Trackers[r.s(3)], &s.Interview))
		switch r.s(5) { // the reset-from-IDLE cases
		case "bare":
			s.CheckEpoch, s.DcloseRecovery = nil, nil
		case "epoch":
			s.DcloseRecovery = nil
		case "marker":
			s.CheckEpoch = nil
		}
		before, att := compact(t, s), (*attest.Attestation)(nil)
		if r.n(4) >= 0 {
			att = f.Attests[r.n(4)]
		}
		got, wantReason := apply(s, OrchestrateVerb(r.s(1)), att), ""
		if r.n(7) >= 0 {
			wantReason = f.Texts[r.n(7)]
		}
		if compact(t, s) != before {
			t.Errorf("%v: the input changed", r[:6])
		}
		if got.OK != (r.n(6) == 1) || got.Reason != wantReason || string(got.Control) != r.s(10) || got.Noop != (r.n(11) == 1) ||
			(got.State != nil) != (r.n(8) >= 0) || (got.Ledger != nil) != (r.n(9) >= 0) {
			t.Errorf("%v: got %+v, oracle ok %v reason %q control %q noop %v", r[:6], got, r[6], wantReason, r[10], r[11])
			continue
		}
		if got.State != nil {
			var want map[string]any
			must(t, json.Unmarshal([]byte(f.Diffs[r.n(8)]), &want))
			if d := diffKeys(t, s, *got.State); !reflect.DeepEqual(d, want) {
				t.Errorf("%v: changed keys\n got %v\nwant %v", r[:6], d, want)
			}
		}
		if got.Ledger != nil {
			must(t, state.AppendLedger(dir, *got.Ledger))
			wantLedger = append(wantLedger, f.Ledgers[r.n(9)])
		}
	}
	data, err := os.ReadFile(filepath.Join(dir, crwdir.DirName, state.LedgerFile))
	must(t, err)
	if lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n"); !reflect.DeepEqual(lines, wantLedger) {
		for i := range wantLedger {
			if i >= len(lines) || lines[i] != wantLedger[i] {
				t.Fatalf("ledger row %d of %d:\n got %s\nwant %s", i, len(wantLedger), lines[min(i, len(lines)-1)], wantLedger[i])
			}
		}
		t.Fatalf("%d ledger rows, want %d", len(lines), len(wantLedger))
	}
}
