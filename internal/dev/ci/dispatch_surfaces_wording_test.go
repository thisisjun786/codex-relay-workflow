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
		"standalone PABCD with no Linear issue and no CRW execution binding, and bounded helper delegation",
		"A Linear issue with no assignment yet is not this case",
		"An issue with no assignment yet is not unmanaged",
		"a Linear issue or a DAG node, whether the relay already holds an assignment for it or not",
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
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dispatch-surfaces.md lacks %q", want)
		}
	}
	for _, old := range []string{
		"and nothing resumes a parent automatically.",
		"Independent task lanes are separate tasks, so nothing in the system knows two were handed the same issue",
		"in the tree the dispatch assigned to it, not in the coordinator's native cwd",
	} {
		if strings.Contains(text, old) {
			t.Errorf("dispatch-surfaces.md keeps the unscoped wording %q", old)
		}
	}
}

func readSkillText(t *testing.T, rel string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(repoRoot(), filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(data)), " ")
}

// The installed entry points reach the binding check (CRW-1130): a caller that follows crw-pabcd or crw-loop without opening the
// reference must still be sent to a relay child for a Linear issue or DAG node, and the desktop-thread and own-wake rules read as
// unmanaged-work rules there too.
func TestDispatchSurfaces_EntryPointsStartFromTheBinding(t *testing.T) {
	const managed = "dispatch-surfaces.md#dispatch-managed-01"
	cases := []struct {
		file string
		want []string
		old  []string
	}{
		{"plugins/crw/skills/crw-pabcd/SKILL.md", []string{
			"start from the binding",
			managed,
			"Linear issue or DAG node",
			"never a `create_thread` lane",
			"The thread and subagent choice below is for unmanaged work",
			"For unmanaged work, work needing its own branch, checkout or merge/CI lane is thread work",
			"Before every authorized dispatch, read [Dispatch surfaces]",
		}, []string{
			"Before an authorized dispatch that is not obviously one or the other",
			"Work needing its own branch, checkout or merge/CI lane is thread work",
		}},
		{"plugins/crw/skills/crw-loop/SKILL.md", []string{
			"DISPATCH-MANAGED-01 (STRICT)",
			managed,
			"never a `create_thread` lane",
			"DISPATCH-SURFACE-01 (STRICT): for unmanaged work, name the surface",
			"For unmanaged work, a request for parallel branch or worktree lanes",
		}, []string{
			"DISPATCH-SURFACE-01 (STRICT): name the surface before fanning work out",
			"A request for parallel branch or worktree lanes **is** the user request",
		}},
		{"plugins/crw/skills/crw-loop/references/lane-dispatch.md", []string{
			managed,
			"is not handed to a `create_thread` lane",
			"## Nothing wakes the coordinator of unmanaged lanes",
			"A managed run's goal-free parent is resumed by relay delivery",
			"arms no wake of its own",
			"For unmanaged work, fan-out **across branches** belongs to lanes",
		}, []string{
			"## Nothing wakes the coordinator A finished lane",
			"A coordinator that dispatches lanes and ends its turn has arranged nothing: keep the work inside the turn",
			"Fan-out **across branches** belongs to lanes, not to subagents; concurrency",
		}},
	}
	for _, c := range cases {
		text := readSkillText(t, c.file)
		for _, want := range c.want {
			if !strings.Contains(text, want) {
				t.Errorf("%s lacks %q", c.file, want)
			}
		}
		for _, old := range c.old {
			if strings.Contains(text, old) {
				t.Errorf("%s keeps the unscoped wording %q", c.file, old)
			}
		}
	}
}

// The worker-evidence sentence says what the runtime in the tree does (CRW-1130): the SubagentStop gate reads the evidence directory
// of the native cwd, so the instruction must not promise the dispatch-assigned tree unconditionally.
func TestDispatchSurfaces_WorkerEvidenceNamesTheLocationTheGateReads(t *testing.T) {
	text := dispatchSurfacesText(t)
	for _, want := range []string{
		"The SubagentStop gate reads `.crw/evidence` under the coordinator's native cwd",
		"a receipt the worker writes under its assigned tree is not accepted there",
		"have the worker write its receipt under the native cwd's `.crw/evidence` and report that absolute path",
		"Only where the runtime in use reads the tree the dispatch assigned (the evidence-location change of the 10-10 review) is a receipt in the assigned tree accepted",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("dispatch-surfaces.md lacks %q", want)
		}
	}
	for _, old := range []string{
		"The worker's evidence is read in the tree the dispatch assigned to it",
		"keeps the native cwd",
	} {
		if strings.Contains(text, old) {
			t.Errorf("dispatch-surfaces.md keeps the unconditional wording %q", old)
		}
	}
}
