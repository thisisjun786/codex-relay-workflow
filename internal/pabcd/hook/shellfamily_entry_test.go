package hook

import (
	"strings"
	"testing"
)

var shellFamilyEntryFiles = map[string]bool{
	"18-crw-894-shell-family.txt":      true,
	"19-crw-894-eval-1688f7c5.txt":     true,
	"20-crw-894-verifier-4636e20a.txt": true,
	"21-crw-894-verifier-5d41d266.txt": true,
	"22-crw-894-verifier-b7d8826d.txt": true,
}

// CRW-894: the shell family (ash, mksh, hush, the busybox applets) is read like bash and sh, and the conditions that inherit a
// pipe are pinned. TestReproductionRows judges rows/18-crw-894-shell-family.txt through the judging functions; this test
// sends the same rows through the three real hook entry points (HandleWorktreeGuardPreTool, HandleMemoryWriteGate,
// HandleGitHubPostGuard) and checks the answer each gives: a deny envelope or nothing, and for an unreadable program the
// memory gate's own wording.
func TestShellFamilyThroughEntryPoints(t *testing.T) {
	githubPostTempHome(t)
	n := 0
	for _, row := range reproductionRows() {
		if !shellFamilyEntryFiles[row.file] {
			continue
		}
		n++
		row := row
		t.Run(row.id, func(t *testing.T) {
			r := newDelRig(t)
			cwd, root, env := gateScene(t)
			tmp := t.TempDir()
			fill := strings.NewReplacer("{MEMORY}", root, "{CHECKOUT}", r.checkout, "{WORK}", cwd, "{TMP}", tmp)
			reproScene(t, row, cwd, r.checkout, tmp, fill)
			cmd := fill.Replace(row.cmd)
			want := row.want
			if row.hasDeviation {
				want = row.deviation // a recorded deviation from the verdict the issue asks for
			}

			memOut := HandleMemoryWriteGate(gateBash(t, cwd, cmd), env)
			if (memOut != "") != (want[0] == "attempt") {
				t.Errorf("HandleMemoryWriteGate = %q, want attempt=%v: %q", memOut, want[0] == "attempt", cmd)
			}
			if memOut != "" && want == reproClasses["U"] && !strings.Contains(gateDeny(t, memOut), "a program the gate cannot read: ") {
				t.Errorf("HandleMemoryWriteGate for an unreadable program lacks the gate's wording: %q", memOut)
			}
			ghOut := HandleGitHubPostGuard(githubPostShell(t, cwd, cmd))
			if (ghOut != "") != (want[1] == "deny") {
				t.Errorf("HandleGitHubPostGuard = %q, want deny=%v: %q", ghOut, want[1] == "deny", cmd)
			}
			wtOut := HandleWorktreeGuardPreTool(wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash",
				"cwd": r.checkout, "tool_input": map[string]any{"command": cmd}}), r.env())
			if (wtOut != "") != (want[2] == "deny") {
				t.Errorf("HandleWorktreeGuardPreTool = %q, want deny=%v: %q", wtOut, want[2] == "deny", cmd)
			}
		})
	}
	if n == 0 {
		t.Fatal("no CRW-894 shell family rows")
	}
}
