package attest

import (
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// The 18 B-class tests of CXC v0.2.40 pabcd-state/test/attest.test.ts, one Test function per oracle test and the same
// assertions. A reason is matched against its CRW wording where the oracle's text names a cxc command.

func at(from, to, did string) *Attestation {
	return &Attestation{From: state.Phase(from), To: state.Phase(to), Did: did}
}

func num(v float64) *float64 { return &v }

func validate(a *Attestation) Result { return Validate(a.From, a.To, a) }

func mustHave(t *testing.T, r Result, want ...string) {
	t.Helper()
	if r.OK {
		t.Fatalf("accepted, want a refusal naming %q", want)
	}
	for _, w := range want {
		if !strings.Contains(r.Reason, w) {
			t.Errorf("reason %q lacks %q", r.Reason, w)
		}
	}
}

func audit(did, output, verdict, residual string) *Attestation {
	a := at("A", "B", did)
	a.AuditOutput, a.AuditVerdict, a.AuditResidual = output, verdict, residual
	return a
}

// advances asserts that an A>B attest claiming pass over output is accepted.
func advances(t *testing.T, output string) {
	t.Helper()
	if r := validate(audit("audited the plan", output, "pass", "")); !r.OK {
		t.Errorf("refused %q: %s", output, r.Reason)
	}
}

func TestAllFourForwardEdgesAreGated(t *testing.T) {
	if got := slices.Sorted(slices.Values(GatedTransitions())); !slices.Equal(got, []string{"A>B", "B>C", "C>D", "P>A"}) {
		t.Fatalf("gated edges %v", got)
	}
	for _, e := range [][2]state.Phase{{"IDLE", "P"}, {"C", "B"}, {"C", "P"}, {"D", "IDLE"}} {
		if IsGated(e[0], e[1]) || !Validate(e[0], e[1], nil).OK {
			t.Errorf("%s>%s is gated", e[0], e[1])
		}
	}
}

func TestPToAAndBToCNeedASpecificDid(t *testing.T) {
	for _, e := range [][2]string{{"P", "A"}, {"B", "C"}} {
		if Validate(state.Phase(e[0]), state.Phase(e[1]), nil).OK || validate(at(e[0], e[1], "tbd")).OK || !validate(at(e[0], e[1], "wrote the plan")).OK {
			t.Errorf("%s>%s did gate", e[0], e[1])
		}
	}
}

func TestAToBNeedsADid(t *testing.T) {
	if Validate("A", "B", nil).OK || validate(at("A", "B", "")).OK || validate(at("A", "B", "done")).OK {
		t.Error("A>B accepted a missing or placeholder did")
	}
}

func TestAToBNeedsAuditOutputAndVerdict(t *testing.T) {
	mustHave(t, validate(at("A", "B", "audited the plan")), "auditOutput")
	mustHave(t, validate(audit("audited the plan", "reviewer: GO-WITH-FIXES; 2 blockers folded back", "", "")), "auditVerdict")
	if !validate(audit("audited the plan", "reviewer: GO-WITH-FIXES; 2 blockers folded back", "near-pass", "2 blockers folded back: (1) rollback gap -> plan amended, (2) phantom constant -> rebutted")).OK {
		t.Error("near-pass with a residual was refused")
	}
}

func TestAToBFailVerdictIsRefusedWithReauditGuidance(t *testing.T) {
	mustHave(t, validate(audit("audited the plan", "VERDICT: FAIL", "fail", "")), "SAME reviewer", "LOOP-REPAIR-01")
}

func TestAToBNearPassNeedsAResidual(t *testing.T) {
	mustHave(t, validate(audit("audited the plan", "VERDICT: GO-WITH-FIXES (blockers=1)", "near-pass", "")), "auditResidual")
}

func TestAToBPassWithCleanOutputAdvances(t *testing.T) { advances(t, "review complete\nVERDICT: PASS") }

func TestAToBFailTailContradictionIsRefused(t *testing.T) {
	mustHave(t, validate(audit("audited the plan", "findings fixed\nVERDICT: FAIL", "pass", "")), "contradict")
}

func TestAToBMidTextFailMentionDoesNotTripTheTail(t *testing.T) {
	advances(t, "scanned for FAIL markers; none apply\nVERDICT: PASS")
}

func TestAToBEarlierFailCorrectedByFinalPassDoesNotTrip(t *testing.T) {
	advances(t, "VERDICT: FAIL\nround 2 after fixes:\nVERDICT: PASS")
}

func TestHasFailVerdictTailSeesOnlyTheLastVerdictLineOfTheFinalFive(t *testing.T) {
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"notes\nVERDICT: FAIL", true},
		{"scanned for FAIL markers; none apply\nVERDICT: PASS", false},
		{"notes\nverdict = fail", true},
		{"VERDICT: FAIL\n1\n2\n3\n4\n5", false},
		{"VERDICT: FAIL\nround 2 after fixes:\nVERDICT: PASS", false},
	} {
		if got := HasFailVerdictTail(c.in); got != c.want {
			t.Errorf("%q: %v", c.in, got)
		}
	}
}

func TestAToBRefusesMismatchedFromAndTo(t *testing.T) {
	if Validate("A", "B", at("C", "D", "x")).OK {
		t.Error("accepted")
	}
}

func TestCToDNeedsDidCheckOutputAndAPassingExitCode(t *testing.T) {
	c := func(out string, exit *float64) *Attestation {
		a := at("C", "D", "ran npm test")
		a.CheckOutput, a.ExitCode = out, exit
		return a
	}
	if validate(c("", nil)).OK || validate(c("77 pass", num(1))).OK || !validate(c("77 pass", num(0))).OK {
		t.Error("C>D gate")
	}
	mustHave(t, validate(c("77 pass", nil)), "exitCode")
	// A direct caller can pass a value JSON cannot carry; the oracle prints it as a template literal does.
	for exit, want := range map[float64]string{math.NaN(): "exitCode NaN.", math.Inf(1): "exitCode Infinity.", math.Inf(-1): "exitCode -Infinity."} {
		mustHave(t, validate(c("77 pass", num(exit))), want)
	}
}

func obj(kv ...any) map[string]any {
	m := map[string]any{}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	return m
}

func TestCoerceValidatesShape(t *testing.T) {
	if Coerce(nil) != nil || Coerce(obj("from", 1.0, "to", "B", "did", "x")) != nil || Coerce([]any{}) != nil {
		t.Error("accepted a bad shape")
	}
	if a := Coerce(obj("from", "A", "to", "B", "did", "  trimmed  ", "exitCode", 0.0)); a == nil || a.Did != "trimmed" || a.ExitCode == nil || *a.ExitCode != 0 {
		t.Errorf("%+v", a)
	}
}

func TestCoerceCarriesTrimmedPlanUnitAndPlanPaths(t *testing.T) {
	a := Coerce(obj("from", "P", "to", "A", "did", "x", "planUnit", "  devlog/_plan/260714_slug  ",
		"planPaths", []any{"  devlog/_plan/260714_slug/010_x.md ", 42.0, ""}))
	if a.PlanUnit != "devlog/_plan/260714_slug" || !slices.Equal(a.PlanPaths, []string{"devlog/_plan/260714_slug/010_x.md"}) {
		t.Errorf("%+v", a)
	}
	if b := Coerce(obj("from", "P", "to", "A", "did", "x", "planUnit", 7.0, "planPaths", "not-an-array")); b.PlanUnit != "" || b.PlanPaths != nil {
		t.Errorf("%+v", b)
	}
}

func TestValidateWorkPhaseBinding(t *testing.T) {
	wp2, none := "wp2", (*string)(nil)
	if !ValidateWorkPhaseBinding(nil, none).OK || !ValidateWorkPhaseBinding(at("P", "A", "x"), none).OK {
		t.Error("an unbound session was refused")
	}
	mustHave(t, ValidateWorkPhaseBinding(nil, &wp2), "workPhaseId", "LOOP-UNIT-CHAIN-01")
	bad := at("B", "C", "x")
	bad.WorkPhaseID = "wp9"
	mustHave(t, ValidateWorkPhaseBinding(bad, &wp2), "wp9", "wp2")
	bad.WorkPhaseID = "wp2"
	if r := ValidateWorkPhaseBinding(bad, &wp2); !r.OK || r.Reasons != nil {
		t.Errorf("%+v", r)
	}
	if got := Coerce(obj("from", "B", "to", "C", "did", "x", "workPhaseId", " wp2 ")).WorkPhaseID; got != "wp2" {
		t.Errorf("%q", got)
	}
}

func TestCoerceCarriesTrimmedAuditFields(t *testing.T) {
	a := Coerce(obj("from", "A", "to", "B", "did", "x", "auditOutput", "  verdict tail  ", "auditVerdict", " NEAR-PASS ", "auditResidual", "  residuals folded  ", "auditRounds", 2.0))
	if a.AuditOutput != "verdict tail" || a.AuditVerdict != "near-pass" || a.AuditResidual != "residuals folded" || a.AuditRounds == nil || *a.AuditRounds != 2 {
		t.Errorf("%+v", a)
	}
	if b := Coerce(obj("from", "A", "to", "B", "did", "x", "auditOutput", 42.0, "auditRounds", "2")); b.AuditOutput != "" || b.AuditRounds != nil {
		t.Errorf("%+v", b)
	}
}

func TestCoerceCarriesATypedOverride(t *testing.T) {
	if !Coerce(obj("from", "I", "to", "P", "did", "accept", "override", true)).Override {
		t.Error("override dropped")
	}
	if Coerce(obj("from", "I", "to", "P", "did", "x", "override", "yes")).Override || Coerce(obj("from", "I", "to", "P", "did", "x")).Override {
		t.Error("a non-boolean override was kept")
	}
}
