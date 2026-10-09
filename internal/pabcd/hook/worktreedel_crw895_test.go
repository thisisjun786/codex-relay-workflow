package hook

import (
	"strings"
	"testing"
)

// CRW-895: find -delete, find -exec/-execdir/-ok/-okdir and xargs deletions. Every shape the issue and the 10-09 re-check list is
// judged through the three public entry points (HandleWorktreeGuardPreTool, HandleMemoryWriteGate, HandleGitHubPostGuard);
// testdata/shellir/rows/18-crw-895.txt holds the same shapes as reproduction rows with a verdict per gate.

// crw895Shape is one command: deny is the worktree guard's verdict; refusedEverywhere marks a command the reader cannot prove, which
// the memory gate and the GitHub guard refuse too.
type crw895Shape struct {
	cmd               string
	deny              bool
	refusedEverywhere bool
}

func crw895Shapes() []crw895Shape {
	return []crw895Shape{
		// the issue: deny
		{"find ../repo -delete", true, false},
		{"find .. -maxdepth 0 -name repo -exec rm -rf {} +", true, false},
		{"echo ../repo | xargs rm -rf", true, false},
		{`find "$PWD" -exec rm -rf {} +`, true, true},
		{"find .. -delete", true, false},
		{`printf '%s\n' ../repo | xargs rm -rf`, true, false},
		{`echo "$X" | xargs rm`, true, false},
		// the issue: allow
		{"find . -name '*.o' -delete", false, false},
		{"find build -delete", false, false},
		{"find . -type f -name '*.tmp' -exec rm {} +", false, false},
		{"find . -name '*.o' | xargs rm", false, false},
		{"git ls-files -z | xargs -0 rm -f", false, false},
		{"git ls-files -z | xargs -0 rm", false, false},
		{"ls | xargs rm", false, false},
		// the 10-09 re-check
		{"find . -type f -delete", false, false},
		{"find . -newer x -delete", false, false},
		{"find ../repo -path '*/build/*' -delete", false, false},
		{"find .. -exec rmdir {} +", true, false},
		{"find ../repo -exec shred {} +", true, false},
		{`find ../repo -exec unlink {} \;`, true, false},
		{`find ../repo -exec git worktree remove {} \;`, true, false},
		{"echo ../repo | xargs git worktree remove", true, false},
		{"echo ../repo | xargs rm", true, false},
		{"echo ../repo | xargs rmdir", true, false},
		{"echo ../repo | xargs unlink", true, false},
		{"echo ../repo | xargs shred", true, false},
		// the independent verifier
		{`find ../repo -okdir rm -rf {} \;`, true, false},
		{`echo "$(cat list.txt)" | xargs rm`, true, false},
		{"echo ~ | xargs rm", true, false},
		{"echo ../repo | parallel git worktree remove", true, true},
		{"echo ../repo | parallel rmdir", true, true},
		{"echo ../repo | parallel unlink", true, true},
		{"echo ../repo | parallel shred", true, true},
	}
}

func TestCRW895ShapesThroughTheEntryPoints(t *testing.T) {
	githubPostTempHome(t)
	r := newDelRig(t)
	cwd, root, env := gateScene(t)
	_ = root
	for _, s := range crw895Shapes() {
		payload := wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": r.checkout,
			"tool_input": map[string]any{"command": s.cmd}})
		got := HandleWorktreeGuardPreTool(payload, r.env())
		switch {
		case s.deny && !strings.Contains(got, "WORKTREE-GUARD-03"):
			t.Errorf("%q: the worktree guard answered %q, want a WORKTREE-GUARD-03 deny", s.cmd, got)
		case !s.deny && got != "":
			t.Errorf("%q: the worktree guard denied: %s", s.cmd, got)
		}
		gate := gatePayload(t, cwd, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": s.cmd}})
		mem := HandleMemoryWriteGate(gate, env)
		post := HandleGitHubPostGuard(wtPayload(t, map[string]any{"hook_event_name": "PreToolUse", "tool_name": "Bash", "cwd": cwd,
			"tool_input": map[string]any{"command": s.cmd}}))
		if s.refusedEverywhere {
			if mem == "" || post == "" {
				t.Errorf("%q: memory gate %q, GitHub guard %q; the reader cannot prove it, so both refuse", s.cmd, mem, post)
			}
			continue
		}
		if mem != "" || post != "" {
			t.Errorf("%q: memory gate %q, GitHub guard %q; neither has reason to touch it", s.cmd, mem, post)
		}
	}
}

// TestCRW895DenyTextIsTheWorktreeGuardWording: the verdict of a find or xargs removal is the WORKTREE-GUARD-03 text of a direct
// removal and names the shape.
func TestCRW895DenyTextIsTheWorktreeGuardWording(t *testing.T) {
	r := newDelRig(t)
	for cmd, what := range map[string]string{
		"find ../repo -delete":                     "find ../repo -delete",
		"find .. -exec rmdir {} +":                 "find .. -exec rmdir",
		"echo ../repo | xargs git worktree remove": "xargs git worktree remove ../repo",
		"xargs -a list.txt rm":                     "xargs rm",
	} {
		v := r.verdict(cmd)
		if !v.Deny || !strings.HasPrefix(v.Reason, "[crw: WORKTREE-GUARD-03] blocked `"+what) {
			t.Errorf("%q: %+v, want a WORKTREE-GUARD-03 deny naming %q", cmd, v, what)
		}
	}
}

// TestCRW895ShapesFromOtherDirectories: the start of a find and the names of xargs are taken from the directory the command runs
// in, so the same shape is judged from a subdirectory of the worktree, and an unknown directory refuses.
func TestCRW895ShapesFromOtherDirectories(t *testing.T) {
	r := newDelRig(t)
	id := r.id()
	sub := r.checkout + "/sub"
	wtWrite(t, sub+"/keep", "x")
	for cmd, deny := range map[string]bool{
		"find ../.. -name x -delete":          true,  // the slot root
		"find .. -delete":                     true,  // the checkout, which holds the directory the command runs in, no test
		"find .. -name x -delete":             false, // the same with a test
		"find . -delete":                      true,  // the directory itself, no test
		"find . -name x -delete":              false, // a test
		"find sub -delete":                    false,
		"echo .. | xargs rm":                  true,
		"echo . | xargs rm":                   false,
		`cd "$X"; find build -name x -delete`: true,
	} {
		if got := evaluateCommand(cmd, sub, id); got.Deny != deny {
			t.Errorf("%q from %s: deny = %v, want %v (%s)", cmd, sub, got.Deny, deny, got.Reason)
		}
	}
}
