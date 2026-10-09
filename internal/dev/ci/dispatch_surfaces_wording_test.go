//go:build dev

package ci

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The reusable dispatch reference starts from the CRW binding (CRW-1130): a relay-held issue or DAG node is routed through crw-run,
// not through desktop threads, a hand-armed wake or a second manifest, while standalone PABCD and bounded helper delegation keep
// the thread/subagent choice, and an unresolved binding is never read as permission for the unmanaged route. These checks pin the
// wording the scenario review of the issue rests on.

const dispatchSurfacesRef = "plugins/crw/skills/crw-pabcd/references/dispatch-surfaces.md"

func dispatchSurfacesText(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(dispatchSurfacesRef)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

func TestDispatchSurfaces_ManagedBindingComesFirst(t *testing.T) {
	text := dispatchSurfacesText(t)
	i := strings.Index(text, "## DISPATCH-MANAGED-01 (STRICT)")
	if i < 0 {
		t.Fatal("no DISPATCH-MANAGED-01 section")
	}
	if first := strings.Index(text, "## DISPATCH-SURFACE-01"); first < i {
		t.Fatal("DISPATCH-MANAGED-01 does not come before DISPATCH-SURFACE-01")
	}
	section := text[i:]
	if j := strings.Index(section[3:], " ## "); j >= 0 {
		section = section[:j+3]
	}
	for _, want := range []string{
		"independent relay child",
		"never a desktop `create_thread` lane",
		"refuses a second active or paused assignment of the same issue",
		"a release past capacity",
		"read-only and review helpers only",
		"read-only projection of the relay assignments and DAG records",
		"resumed by relay delivery",
		"yields only after that readiness is established",
		"standalone PABCD with no relay-held issue, and bounded helper delegation",
		"not permission to take the unmanaged route",
		"a bounded helper or worktree worker needs no relay binding",
		"never an independent child's assignment, a DAG acceptance or a relay receipt",
	} {
		if !strings.Contains(section, want) {
			t.Errorf("DISPATCH-MANAGED-01 lacks %q", want)
		}
	}
}

func TestDispatchSurfaces_UnmanagedRulesAreScoped(t *testing.T) {
	text := dispatchSurfacesText(t)
	for _, want := range []string{
		"for unmanaged lanes nothing resumes a parent automatically",
		"For unmanaged work, independent task lanes are separate tasks",
		"For unmanaged work, N independent task lanes mean N `worktree` threads",
		"A CRW-bound issue or DAG node is routed by",
		"a managed lane is started by the relay",
		"arranges no second wake or manifest",
		"in the tree the dispatch assigned to it, not in the coordinator's native cwd",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dispatch-surfaces.md lacks %q", want)
		}
	}
	for _, old := range []string{
		"and nothing resumes a parent automatically.",
		"Independent task lanes are separate tasks, so nothing in the system knows two were handed the same issue",
	} {
		if strings.Contains(text, old) {
			t.Errorf("dispatch-surfaces.md keeps the unscoped wording %q", old)
		}
	}
}
