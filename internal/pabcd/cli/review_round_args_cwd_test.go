package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// A working directory spelled "<root>/alias/.." is the directory the OS reaches, not the one path.resolve's cleaning names:
// the kernel resolves "alias" to <root>/ws/deep and then ".." to <root>/ws, while cleaning lands on <root>. An entry that is
// inside only the cleaned path is outside the real working directory, so it reads "missing" (CRW-649).
func TestReviewRoundCwdDotDotIsTheDirectoryTheOsReaches(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	reviewRoundArgsWrite(t, filepath.Join(root, "out", "000_secret.md"), "TOP SECRET\n")
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, "ws", "deep"), 0o755))
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws", "deep"), filepath.Join(root, "alias")))

	got := Recomputed(root+"/alias/..", []goalplan.PlanFileHash{{Path: "out/000_secret.md"}})
	if len(got) != 1 || got[0].Sha256 != "missing" {
		t.Errorf("an entry outside the real working directory reads %v, want missing", got)
	}
}

// The collection refuses an entry whose real path lies outside the working directory the OS reaches, even when the entry is
// spelled so that path.resolve's cleaned working directory contains it (CRW-649).
func TestReviewRoundCwdCollectRefusesAnEntryOutsideTheRealWorkingDirectory(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	reviewRoundArgsWrite(t, filepath.Join(root, "out", "000_secret.md"), "TOP SECRET\n")
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, "ws", "deep"), 0o755))
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, "ws", "u"), 0o755))
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws", "deep"), filepath.Join(root, "alias")))
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "out"), filepath.Join(root, "ws", "u", "000_escape")))

	entry := filepath.Join(root, "ws", "u", "000_escape", "000_secret.md")
	files, refusal, err := reviewRoundArgsCollectPlanFiles(root+"/alias/..", filepath.Join(root, "ws", "u"), []string{entry})
	if want := "plan path " + entry + " is not a readable regular file"; err != nil || len(files) != 0 || refusal != want {
		t.Errorf("%v %q %v, want refusal %q", files, refusal, err, want)
	}
}

// The stored key is the plan-unit spelling when neither the working directory's spelling nor its real path can name the
// entry: the unit's parent relative to the real working directory, the unit's own base name, and the entry below the unit.
// With an aliased plan unit whose link points at the document, the key keeps that link, so repointing the link changes what
// Recomputed reads (CRW-649).
func TestReviewRoundCwdKeyKeepsThePlanUnitSpelling(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	for _, dir := range []string{"a", "b"} {
		reviewRoundArgsWrite(t, filepath.Join(root, "ws", dir, "000_plan.md"), "plan "+dir+"\n")
	}
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws"), filepath.Join(root, "alias")))
	reviewRoundArgsMust(t, os.Symlink("a", filepath.Join(root, "ws", "unit")))

	cwd, unit := filepath.Join(root, "ws"), "../alias/unit"
	files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, []string{unit + "/000_plan.md"})
	if want := []goalplan.PlanFileHash{{Path: "unit/000_plan.md", Sha256: reviewRoundArgsHex("plan a\n")}}; err != nil || refusal != "" || !slices.Equal(files, want) {
		t.Fatalf("%v %q %v, want %v", files, refusal, err, want)
	}
	reviewRoundArgsMust(t, os.Remove(filepath.Join(root, "ws", "unit")))
	reviewRoundArgsMust(t, os.Symlink("b", filepath.Join(root, "ws", "unit")))
	if got := Recomputed(cwd, files); len(got) != 1 || got[0].Sha256 != reviewRoundArgsHex("plan b\n") {
		t.Errorf("the key no longer follows the link of the unit: %v", got)
	}
}

// A chain of more than forty links inside the workspace, ending at the same document: the kernel's stat refuses the path
// (ELOOP), so Recomputed reads "missing" as the oracle's readFileSync does, where following the links with EvalSymlinks
// would hash it (CRW-649).
func TestReviewRoundCwdLinkChainPastTheKernelLimitIsMissing(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	reviewRoundArgsWrite(t, filepath.Join(root, "ws", "unit", "000_plan.md"), "# plan\n")
	prev := filepath.Join(root, "ws", "unit")
	for i := 0; i < 42; i++ {
		next := filepath.Join(root, "ws", "l"+strconv.Itoa(i))
		reviewRoundArgsMust(t, os.Symlink(prev, next))
		prev = next
	}
	got := Recomputed(filepath.Join(root, "ws"), []goalplan.PlanFileHash{{Path: "l41/000_plan.md"}})
	if len(got) != 1 || got[0].Sha256 != "missing" {
		t.Errorf("a chain past the kernel's link limit reads %v, want missing", got)
	}
}

// A link between the plan unit and the point where the path enters the working directory stays in the stored key, so
// repointing it changes what Recomputed reads (CRW-649).
func TestReviewRoundCwdKeyKeepsTheLinkAboveThePlanUnit(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	for _, dir := range []string{"a", "b"} {
		reviewRoundArgsWrite(t, filepath.Join(root, "ws", dir, "u", "000_plan.md"), "plan "+dir+"\n")
	}
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws"), filepath.Join(root, "alias")))
	reviewRoundArgsMust(t, os.Symlink("a", filepath.Join(root, "ws", "link")))
	cwd := filepath.Join(root, "ws")
	files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, "../alias/link/u", []string{"../alias/link/u/000_plan.md"})
	if want := []goalplan.PlanFileHash{{Path: "link/u/000_plan.md", Sha256: reviewRoundArgsHex("plan a\n")}}; err != nil || refusal != "" || !slices.Equal(files, want) {
		t.Fatalf("%v %q %v, want %v", files, refusal, err, want)
	}
	reviewRoundArgsMust(t, os.Remove(filepath.Join(root, "ws", "link")))
	reviewRoundArgsMust(t, os.Symlink("b", filepath.Join(root, "ws", "link")))
	if got := Recomputed(cwd, files); len(got) != 1 || got[0].Sha256 != reviewRoundArgsHex("plan b\n") {
		t.Errorf("the key no longer follows the link above the unit: %v", got)
	}
}

// A working directory spelled with a link and ".." whose target lies outside its cleaned parent: the stored key resolves
// through Recomputed to the same document, or the entry is refused — never a key that reads missing (CRW-649).
func TestReviewRoundCwdKeyResolvesThroughTheSameSpelling(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	reviewRoundArgsWrite(t, filepath.Join(root, "x", "ws", "u", "000_plan.md"), "# plan\n")
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, "x", "ws", "deep"), 0o755))
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, "r"), 0o755))
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "x", "ws", "deep"), filepath.Join(root, "r", "alias")))
	cwd := root + "/r/alias/.."
	unit := filepath.Join(root, "x", "ws", "u")
	files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, []string{unit + "/000_plan.md"})
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if refusal != "" {
		t.Logf("the entry is refused with %q", refusal)
		return
	}
	if len(files) != 1 {
		t.Fatalf("collect returned %v", files)
	}
	if got := Recomputed(cwd, files); len(got) != 1 || got[0].Sha256 != reviewRoundArgsHex("# plan\n") {
		t.Errorf("the stored key %q reads %v, want the plan's hash (never missing)", files[0].Path, got)
	}
}

// A relative working directory spelled with a link and ".." is the directory the OS reaches, not the cleaned one: an entry
// inside only the cleaned path reads missing (CRW-649).
func TestReviewRoundCwdRelativeDotDotIsTheDirectoryTheOsReaches(t *testing.T) {
	root := reviewRoundArgsRealTemp(t)
	reviewRoundArgsWrite(t, filepath.Join(root, "out", "000_secret.md"), "TOP SECRET\n")
	reviewRoundArgsMust(t, os.MkdirAll(filepath.Join(root, "ws", "deep"), 0o755))
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws", "deep"), filepath.Join(root, "alias")))
	t.Chdir(root)
	got := Recomputed("alias/..", []goalplan.PlanFileHash{{Path: "out/000_secret.md"}})
	if len(got) != 1 || got[0].Sha256 != "missing" {
		t.Errorf("a relative link-and-dot-dot cwd reads %v, want missing", got)
	}
}
