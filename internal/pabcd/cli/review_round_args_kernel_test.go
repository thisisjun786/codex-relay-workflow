package cli

import (
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// reviewRoundKernelLinkChain builds <root>/ws/u/000_plan.md, a chain of n links <root>/ws/l0 -> u,
// l1 -> l0, ... l<n-1> -> l<n-2>, and a link <root>/wslink -> ws, and returns the root. A key spelled
// against cwd <root>/wslink crosses the kernel's symlink limit once n reaches forty (one link for the
// working directory itself plus the n links), while the same chain spelled absolutely does not.
func reviewRoundKernelLinkChain(t *testing.T, n int) string {
	t.Helper()
	root := reviewRoundArgsRealTemp(t)
	reviewRoundArgsWrite(t, filepath.Join(root, "ws", "u", "000_plan.md"), "# plan\n")
	prev := filepath.Join(root, "ws", "u")
	for i := 0; i < n; i++ {
		next := filepath.Join(root, "ws", "l"+strconv.Itoa(i))
		reviewRoundArgsMust(t, os.Symlink(prev, next))
		prev = next
	}
	reviewRoundArgsMust(t, os.Symlink(filepath.Join(root, "ws"), filepath.Join(root, "wslink")))
	return root
}

// A cwd-joined key candidate that passes EvalSymlinks but exceeds the kernel's symlink limit is not
// stored: the collection refuses the entry, because Recomputed would read the stored key with os.Stat
// and answer "missing" although the file is unchanged, and the round would report StalePlan (CRW-673).
func TestReviewRoundKernelCandidatePastTheKernelLinkLimitIsRefused(t *testing.T) {
	root := reviewRoundKernelLinkChain(t, 40)
	unit, entry := filepath.Join(root, "ws", "l39"), filepath.Join(root, "ws", "l39", "000_plan.md")
	files, refusal, err := reviewRoundArgsCollectPlanFiles(filepath.Join(root, "wslink"), unit, []string{entry})
	if want := "plan path " + entry + " is not a readable regular file"; err != nil || len(files) != 0 || refusal != want {
		t.Errorf("%v %q %v, want refusal %q", files, refusal, err, want)
	}
}

// A cwd-joined key candidate within the kernel's symlink limit is stored, and Recomputed reads the same
// hash through it: the added os.Stat gate does not refuse a spelling the kernel can resolve (CRW-673).
func TestReviewRoundKernelCandidateWithinTheKernelLinkLimitStoresTheKey(t *testing.T) {
	root := reviewRoundKernelLinkChain(t, 39)
	cwd, unit := filepath.Join(root, "wslink"), filepath.Join(root, "ws", "l38")
	files, refusal, err := reviewRoundArgsCollectPlanFiles(cwd, unit, []string{filepath.Join(unit, "000_plan.md")})
	want := []goalplan.PlanFileHash{{Path: "l38/000_plan.md", Sha256: reviewRoundArgsHex("# plan\n")}}
	if err != nil || refusal != "" || !slices.Equal(files, want) {
		t.Fatalf("%v %q %v, want %v", files, refusal, err, want)
	}
	if got := Recomputed(cwd, files); !slices.Equal(got, files) {
		t.Errorf("Recomputed read %v, want the stored hash", got)
	}
}
