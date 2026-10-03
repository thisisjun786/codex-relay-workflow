package attest

import (
	"os"
	"path/filepath"
	"testing"
)

// The 3 B-class tests of CXC v0.2.40 test/plan-gate.test.ts; /cxc plan init/ becomes its CRW name, crw pabcd plan init.

func plan(unit string, paths ...string) *Attestation {
	return &Attestation{From: "P", To: "A", Did: "x", PlanUnit: unit, PlanPaths: paths}
}

func write(t *testing.T, path string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Dir(path), 0o755))
	must(t, os.WriteFile(path, []byte("x\n"), 0o644))
}

func reasonHas(t *testing.T, r PlanResult, want ...string) {
	t.Helper()
	mustHave(t, Result{OK: r.OK, Reason: r.Reason}, want...)
}

func TestPlanGateRefusesANullAttestAndAMissingPlanUnitWithAScaffoldHint(t *testing.T) {
	cwd := t.TempDir()
	reasonHas(t, ValidatePlanArtifacts(nil, cwd), "planUnit", "crw pabcd plan init")
	reasonHas(t, ValidatePlanArtifacts(&Attestation{From: "P", To: "A", Did: "x"}, cwd), "DIFFLEVEL-ROADMAP-01")
}

func TestPlanGateRefusesAMissingDirAndADirWithoutNumberedDocs(t *testing.T) {
	cwd := t.TempDir()
	reasonHas(t, ValidatePlanArtifacts(plan("devlog/_plan/000000_none"), cwd), "does not exist")
	write(t, filepath.Join(cwd, "devlog", "_plan", "000000_empty", "PLAN.md")) // LEXICO-SPLIT-01 violation
	reasonHas(t, ValidatePlanArtifacts(plan("devlog/_plan/000000_empty"), cwd), "no numbered plan docs")
}

func TestPlanGateAcceptsAValidUnitAndVerifiesPlanPathsOnDisk(t *testing.T) {
	cwd := t.TempDir()
	unit := filepath.Join(cwd, "devlog", "_plan", "000000_ok")
	write(t, filepath.Join(unit, "000_plan.md"))
	write(t, filepath.Join(unit, "010_phase1.md"))
	if r := ValidatePlanArtifacts(plan("devlog/_plan/000000_ok"), cwd); !r.OK || r.Unit != "devlog/_plan/000000_ok" {
		t.Errorf("%+v", r)
	}
	if r := ValidatePlanArtifacts(plan("devlog/_plan/000000_ok", "devlog/_plan/000000_ok/010_phase1.md"), cwd); !r.OK {
		t.Errorf("%+v", r)
	}
	reasonHas(t, ValidatePlanArtifacts(plan("devlog/_plan/000000_ok", "devlog/_plan/000000_ok/999_ghost.md"), cwd), "999_ghost.md does not exist")
}
