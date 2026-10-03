package attest

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
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

// Where a relative path needs process.cwd() and the directory is deeper than getcwd answers, the oracle throws (ERANGE); the
// port refuses at the same point (an absolute unit that is missing is refused first, as the oracle does).
func TestAnUnreadableWorkingDirectoryRefusesWhereTheOracleThrows(t *testing.T) {
	t.Chdir(t.TempDir())
	for i := 0; i < 24; i++ {
		must(t, os.Mkdir(strings.Repeat("d", 200), 0o755))
		must(t, os.Chdir(strings.Repeat("d", 200)))
	}
	write(t, "unit/000_plan.md")
	reasonHas(t, ValidatePlanArtifacts(plan("unit"), "."), "working directory")
	reasonHas(t, ValidatePlanArtifacts(plan("/nonexistent/unit"), "."), "does not exist")
}

// The workspace the gate confines to is the working directory it is called with, symlinks followed on both sides. The oracle
// accepted any existing directory or path (CRW-425: a departure by decision, a review finding of kind security). A refusal names the
// entry as written, never a real path.

// confinement is root/ws, the workspace, holding unit/000_plan.md and unit/010_a.md; root/outside, a unit and a file the gate must
// not accept; root/ws2, a sibling whose name begins with the workspace's; and 000_r.md beside them.
type confinement struct{ root, cwd, outside string }

func newConfinement(t *testing.T) confinement {
	t.Helper()
	root := t.TempDir()
	c := confinement{root: root, cwd: filepath.Join(root, "ws"), outside: filepath.Join(root, "outside")}
	write(t, filepath.Join(c.cwd, "unit", "000_plan.md"))
	write(t, filepath.Join(c.cwd, "unit", "010_a.md"))
	write(t, filepath.Join(c.outside, "000_o.md"))
	write(t, filepath.Join(c.outside, "f.txt"))
	write(t, filepath.Join(root, "000_r.md"))
	write(t, filepath.Join(root, "ws2", "000_s.md"))
	return c
}

func TestPlanGateRefusesAnEntryOutsideTheWorkingDirectory(t *testing.T) {
	c := newConfinement(t)
	must(t, os.Symlink(c.outside, filepath.Join(c.cwd, "link")))
	must(t, os.Symlink(filepath.Join(c.outside, "f.txt"), filepath.Join(c.cwd, "flink")))
	outside := "resolves outside the working directory"
	unit := func(p string) []string { return []string{"planUnit " + p + " ", outside} }
	entry := func(p string) []string { return []string{"planPaths entry " + p + " ", outside} }
	file := filepath.Join(c.outside, "f.txt")
	for _, tc := range []struct {
		name string
		att  *Attestation
		want []string
	}{
		{"an absolute unit", plan(c.outside), unit(c.outside)},
		{"a unit that climbs out with ..", plan("../outside"), unit("../outside")},
		{"the parent as the unit", plan(".."), unit("..")},
		{"a file outside as the unit", plan(file), unit(file)},
		{"a sibling whose name begins with the workspace's", plan(filepath.Join(c.root, "ws2")), unit(filepath.Join(c.root, "ws2"))},
		{"a symlink in the workspace to a directory outside, as the unit", plan("link"), unit("link")},
		{"a symlink followed by .. in an absolute unit", plan(c.cwd + "/link/../ws2"), unit(c.cwd + "/link/../ws2")},
		{"an absolute planPaths entry", plan("unit", file), entry(file)},
		{"a planPaths entry that climbs out with ..", plan("unit", "../outside/f.txt"), entry("../outside/f.txt")},
		{"the root as a planPaths entry", plan("unit", c.root), entry(c.root)},
		{"a planPaths symlink to a directory outside", plan("unit", "link"), entry("link")},
		{"a planPaths path through a symlink outside", plan("unit", "link/f.txt"), entry("link/f.txt")},
		{"a planPaths symlink to a file outside", plan("unit", "flink"), entry("flink")},
		{"an outside entry after an inside one", plan("unit", "unit/010_a.md", "../outside"), entry("../outside")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := ValidatePlanArtifacts(tc.att, c.cwd)
			reasonHas(t, r, tc.want...)
			if strings.Contains(r.Reason, c.outside) && !strings.Contains(strings.Join(tc.want, ""), c.outside) {
				t.Errorf("the reason names a real path: %q", r.Reason)
			}
		})
	}
	// An entry that is missing keeps the oracle's answer, outside or not: existence is judged first.
	reasonHas(t, ValidatePlanArtifacts(plan("../nope"), c.cwd), "planUnit ../nope does not exist")
	reasonHas(t, ValidatePlanArtifacts(plan("unit", "../nope"), c.cwd), "planPaths entry ../nope does not exist on disk")
}

func TestPlanGateAcceptsWhatStaysInsideTheWorkingDirectory(t *testing.T) {
	c := newConfinement(t)
	must(t, os.Symlink(filepath.Join(c.cwd, "unit"), filepath.Join(c.cwd, "alias")))
	must(t, os.Symlink(filepath.Join(c.cwd, "unit", "010_a.md"), filepath.Join(c.cwd, "doclink")))
	write(t, filepath.Join(c.cwd, "..x", "000_plan.md")) // a name that begins with .. is not a climb
	here := t.TempDir()
	write(t, filepath.Join(here, "000_a.md"))
	for _, tc := range []struct {
		name string
		att  *Attestation
		cwd  string
		unit string
	}{
		{"a symlink to a unit inside", plan("alias"), c.cwd, "alias"},
		{"a planPaths symlink to a file inside", plan("unit", "doclink"), c.cwd, "unit"},
		{"a directory named ..x", plan("..x", "..x"), c.cwd, "..x"},
		{"the workspace itself as the unit", plan("."), here, "."},
		{"the workspace as a planPaths entry, spelled four ways", plan(".", "", ".", here, here+"/"), here, "."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if r := ValidatePlanArtifacts(tc.att, tc.cwd); !r.OK || r.Unit != tc.unit {
				t.Errorf("%+v, want OK with unit %q", r, tc.unit)
			}
		})
	}
}

func TestPlanGateJudgesEntriesAgainstTheRealWorkingDirectory(t *testing.T) {
	c := newConfinement(t)
	link := filepath.Join(c.root, "wslink")
	must(t, os.Symlink(c.cwd, link))
	must(t, os.Symlink(c.outside, filepath.Join(c.cwd, "out")))
	for _, tc := range []struct {
		att  *Attestation
		unit string
	}{
		{plan("unit"), "unit"},
		{plan(link + "/unit"), "unit"},
		{plan(c.cwd+"/unit", "unit/010_a.md"), "../ws/unit"}, // the oracle's relative(): lexical, against the path it is given
	} {
		if r := ValidatePlanArtifacts(tc.att, link); !r.OK || r.Unit != tc.unit {
			t.Errorf("%+v through the link %s: %+v, want OK with unit %q", tc.att, link, r, tc.unit)
		}
	}
	reasonHas(t, ValidatePlanArtifacts(plan("../outside"), link), "planUnit ../outside resolves outside")
	reasonHas(t, ValidatePlanArtifacts(plan("out"), link), "planUnit out resolves outside")
	reasonHas(t, ValidatePlanArtifacts(plan("unit", link+"/../outside/f.txt"), link), "resolves outside")
}

func TestPlanGateKeepsTheOraclesExistenceRules(t *testing.T) {
	c := newConfinement(t)
	// 42 links in a row: the kernel stops at 40, so the oracle's statSync refuses the path; EvalSymlinks alone would follow it.
	prev := filepath.Join(c.cwd, "unit")
	for i := 0; i < 42; i++ {
		next := filepath.Join(c.cwd, "l"+strconv.Itoa(i))
		must(t, os.Symlink(prev, next))
		prev = next
	}
	reasonHas(t, ValidatePlanArtifacts(plan(prev), c.cwd), "does not exist")
	reasonHas(t, ValidatePlanArtifacts(plan("unit", prev+"/000_plan.md"), c.cwd), "does not exist on disk")
	if os.Geteuid() == 0 { // root searches every directory
		return
	}
	blocked := filepath.Join(c.cwd, "blocked")
	must(t, os.Mkdir(blocked, 0))
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })
	reasonHas(t, ValidatePlanArtifacts(plan(blocked+"/../unit"), c.cwd), "does not exist")
	reasonHas(t, ValidatePlanArtifacts(plan("unit", blocked+"/../unit/000_plan.md"), c.cwd), "does not exist on disk")
}

func TestPlanGateRefusesWhenTheWorkingDirectoryCannotBeResolved(t *testing.T) {
	c := newConfinement(t)
	unit := filepath.Join(c.cwd, "unit")
	reasonHas(t, ValidatePlanArtifacts(plan(unit), filepath.Join(c.root, "gone")), "working directory")
	a, b := filepath.Join(c.root, "a"), filepath.Join(c.root, "b")
	must(t, os.Symlink(b, a))
	must(t, os.Symlink(a, b))
	reasonHas(t, ValidatePlanArtifacts(plan(unit), a), "working directory")
}

// An existing unit named absolutely needs no process directory, but the working directory it is judged against does: where getcwd
// cannot answer for a relative one, the gate refuses (the base of containment cannot be known) instead of accepting the unit.
func TestPlanGateRefusesAnExistingAbsoluteUnitWhenTheWorkingDirectoryCannotBeRead(t *testing.T) {
	unit := t.TempDir()
	write(t, filepath.Join(unit, "000_plan.md"))
	t.Chdir(t.TempDir())
	for i := 0; i < 24; i++ {
		must(t, os.Mkdir(strings.Repeat("d", 200), 0o755))
		must(t, os.Chdir(strings.Repeat("d", 200)))
	}
	reasonHas(t, ValidatePlanArtifacts(plan(unit), "."), "working directory")
}

// A real path past the system's length limit cannot be named by a path-based check, so an entry that only a shorter spelling through a
// symlink reaches is refused as unresolved, where the oracle's statSync accepts it: containment cannot be shown, and the gate does not
// guess (the refusal is the fail-closed direction; os.Root would answer without the limit).
func TestPlanGateRefusesAnEntryWhoseRealPathIsTooLongToResolve(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	must(t, os.Mkdir(cwd, 0o755))
	write(t, filepath.Join(cwd, "unit", "000_plan.md"))
	t.Chdir(cwd)
	name := strings.Repeat("d", 200)
	short := ""
	for i := 1; i <= 24; i++ {
		must(t, os.Mkdir(name, 0o755))
		must(t, os.Chdir(name))
		if i == 19 {
			wd, err := os.Getwd()
			if err != nil {
				t.Skip("this system cannot name a directory that deep:", err)
			}
			short = wd
		}
	}
	write(t, "000_plan.md")
	must(t, os.Symlink(short, filepath.Join(cwd, "shortcut")))
	entry := cwd + "/shortcut/" + strings.TrimSuffix(strings.Repeat(name+"/", 5), "/") // 1,000 bytes written, 4,800 real
	reasonHas(t, ValidatePlanArtifacts(plan(entry), cwd), "planUnit "+entry+" could not be resolved to a real path")
	reasonHas(t, ValidatePlanArtifacts(plan("unit", entry+"/000_plan.md"), cwd), "planPaths entry "+entry+"/000_plan.md could not be resolved to a real path")
}

// The unit handed back is persisted and read again as resolve(cwd, unit): it must name the directory the gate validated.
func TestPlanGateReturnsTheDirectoryItValidated(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(root, "ws")
	write(t, filepath.Join(cwd, "inner", "unit", "000_plan.md"))
	must(t, os.MkdirAll(filepath.Join(cwd, "inner", "sub"), 0o755))
	must(t, os.Symlink(filepath.Join(cwd, "inner", "sub"), filepath.Join(cwd, "link")))
	write(t, filepath.Join(root, "outside", "000_o.md"))
	validated := filepath.Join(cwd, "inner", "unit")
	named := func(cwd string, r PlanResult) {
		t.Helper()
		path, err := resolve(cwd, r.Unit) // as the caller resolves it
		if got, err2 := filepath.EvalSymlinks(path); !r.OK || err != nil || err2 != nil || got != validated {
			t.Errorf("%+v from %s resolves to %q (%v, %v), want %s", r, cwd, got, err, err2, validated)
		}
	}
	// "link/.." is ws/inner, so the entry validates ws/inner/unit; spelled lexically it is ws/unit, a symlink out (or nothing).
	entry := cwd + "/link/../unit"
	must(t, os.Symlink(filepath.Join(root, "outside"), filepath.Join(cwd, "unit")))
	r := ValidatePlanArtifacts(plan(entry), cwd)
	named(cwd, r)
	if r.Unit != "inner/unit" {
		t.Errorf("unit %q, want inner/unit", r.Unit)
	}
	must(t, os.Remove(filepath.Join(cwd, "unit")))
	r = ValidatePlanArtifacts(plan(entry), cwd)
	named(cwd, r)
	// A working directory spelled "<root>/alias/..": the caller's join cleans it, the real path does not, so only the real path of the
	// unit names it from there.
	phys := filepath.Join(root, "physical")
	write(t, filepath.Join(phys, "inside", "unit", "000_plan.md"))
	must(t, os.MkdirAll(filepath.Join(phys, "inside", "deep"), 0o755))
	must(t, os.Symlink(filepath.Join(phys, "inside", "deep"), filepath.Join(root, "alias")))
	must(t, os.Symlink(filepath.Join(root, "outside"), filepath.Join(root, "unit")))
	odd := root + "/alias/.."
	validated = filepath.Join(phys, "inside", "unit")
	r = ValidatePlanArtifacts(plan(odd+"/unit"), odd)
	named(odd, r)
	if r.Unit != validated {
		t.Errorf("unit %q, want the absolute %s", r.Unit, validated)
	}
}
