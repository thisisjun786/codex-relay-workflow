package hook

import (
	"strings"
	"testing"
)

// CRW-894: the shell family (ash, mksh, hush, the busybox applets) is read like bash and sh, and the conditions that inherit a
// pipe are pinned. TestReproductionRows judges rows/18-crw-894-shell-family.txt through the judging functions; this test
// sends the same rows through the three real hook entry points (HandleWorktreeGuardPreTool, HandleMemoryWriteGate,
// HandleGitHubPostGuard) and checks the answer each gives: a deny envelope or nothing, and for an unreadable program the
// memory gate's own wording.
func TestShellFamilyThroughEntryPoints(t *testing.T) {
	githubPostTempHome(t)
	n := 0
	for _, row := range reproductionRows() {
		if row.file != "18-crw-894-shell-family.txt" {
			continue
		}
		n++
		row := row
		t.Run(row.id, func(t *testing.T) {
			r := newDelRig(t)
			cwd, root, env := gateScene(t)
			fill := strings.NewReplacer("{MEMORY}", root, "{CHECKOUT}", r.checkout, "{WORK}", cwd)
			cmd := fill.Replace(row.cmd)

			memOut := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env)
			if (memOut != "") != (row.want[0] == "attempt") {
				t.Errorf("HandleMemoryWriteGate = %q, want attempt=%v: %q", memOut, row.want[0] == "attempt", cmd)
			}
			if memOut != "" && row.want == reproClasses["U"] && !strings.Contains(gateDeny(t, memOut), "a program the gate cannot read: ") {
				t.Errorf("HandleMemoryWriteGate for an unreadable program lacks the gate's wording: %q", memOut)
			}
			ghOut := HandleGitHubPostGuard(githubPostShell(t, t.TempDir(), cmd))
			if (ghOut != "") != (row.want[1] == "deny") {
				t.Errorf("HandleGitHubPostGuard = %q, want deny=%v: %q", ghOut, row.want[1] == "deny", cmd)
			}
			wtOut := HandleWorktreeGuardPreTool(wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash",
				"cwd": r.checkout, "tool_input": map[string]any{"command": cmd}}), r.env())
			if (wtOut != "") != (row.want[2] == "deny") {
				t.Errorf("HandleWorktreeGuardPreTool = %q, want deny=%v: %q", wtOut, row.want[2] == "deny", cmd)
			}
		})
	}
	if n == 0 {
		t.Fatal("no CRW-894 shell family rows")
	}
}
