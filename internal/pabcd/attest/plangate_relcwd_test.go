package attest

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// A relative --cwd is the directory the OS reaches, not the one filepath.Join's cleaning names: the kernel resolves an alias
// to its target and only then "..", so "alias/.." for an alias to <r>/ws/deep is <r>/ws, while cleaning lands on <r> (CRW-662,
// the same spelling rule as the review-round boundary CRW-649). The gate's boundary is that directory, so a plan unit beside
// it is outside.
func TestPlanGateRelCwdRefusesAUnitOutsideTheDirectoryTheOsReaches(t *testing.T) {
	r := planGateRelCwdRoot(t)
	write(t, filepath.Join(r, "unit", "000_plan.md"))       // beside the directory the OS reaches
	write(t, filepath.Join(r, "ws", "unit", "000_plan.md")) // inside it
	must(t, os.MkdirAll(filepath.Join(r, "ws", "deep"), 0o755))
	must(t, os.Symlink(filepath.Join(r, "ws", "deep"), filepath.Join(r, "alias")))

	outside := filepath.Join(r, "unit")
	reasonHas(t, ValidatePlanArtifacts(plan(outside), "alias/.."),
		"planUnit "+outside+" resolves outside the working directory")
}

// The same relative cwd with a unit inside the directory the OS reaches still passes, and the unit handed back is the one the
// caller resolves against that same cwd.
func TestPlanGateRelCwdAcceptsAUnitInsideTheDirectoryTheOsReaches(t *testing.T) {
	r := planGateRelCwdRoot(t)
	write(t, filepath.Join(r, "ws", "unit", "000_plan.md"))
	must(t, os.MkdirAll(filepath.Join(r, "ws", "deep"), 0o755))
	must(t, os.Symlink(filepath.Join(r, "ws", "deep"), filepath.Join(r, "alias")))

	got := ValidatePlanArtifacts(plan(filepath.Join(r, "ws", "unit")), "alias/..")
	if !got.OK {
		t.Fatalf("%+v, want OK", got)
	}
	reached, err := resolve("alias/..", got.Unit)
	if err != nil {
		t.Fatal(err)
	}
	if real, err := filepath.EvalSymlinks(reached); err != nil || real != filepath.Join(r, "ws", "unit") {
		t.Errorf("unit %q resolves to %q (%v), want %s", got.Unit, real, err, filepath.Join(r, "ws", "unit"))
	}
}

// planGateRelCwdRoot makes a temporary directory the process works in and returns its physical spelling, so "alias/.." names
// the directory the kernel reaches even where the system's temp path is itself a link.
func planGateRelCwdRoot(t *testing.T) string {
	t.Helper()
	t.Chdir(t.TempDir())
	r, err := syscall.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return r
}
