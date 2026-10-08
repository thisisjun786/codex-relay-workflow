package hook

import "testing"

// braceGate names the three gates on one command: the worktree guard denies it, the memory gate makes a write attempt, and
// the GitHub guard denies it. Each gate sees the brace word as unknown, so it refuses or asks.
func braceGate(t *testing.T, cmd string, root, checkout string) (worktree, memory, github bool) {
	t.Helper()
	r := newDelRig(t)
	cwd, memRoot, env := gateScene(t)
	_ = root
	_ = checkout
	worktree = r.verdict(cmd).Deny
	memory = memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env).Surface != ""
	_, github = githubPostJudgeText(cmd, cwd)
	_ = memRoot
	return worktree, memory, github
}

// TestBraceWordsAcrossQuotesRefusedInAllGates: the three commands of the ruling, and the quoted-part shapes, are refused or
// asked for in every gate that reads them.
func TestBraceWordsAcrossQuotesRefusedInAllGates(t *testing.T) {
	r := newDelRig(t)
	cwd, root, env := gateScene(t)
	cases := []string{
		"{rm,\"-rf\"} " + r.checkout,
		"{cp,\"/tmp/x\"} " + root + "/a",
		"{\"gh\",\"pr\"} comment 1 -b plain",
		"{a,'b'} " + root + "/x",
		"{'a',b} " + root + "/x",
		"{$x,b} " + root + "/x",
		"{a,b}$x " + root + "/x",
		"x{\"a\",b}y " + root + "/x",
		"{1..\"3\"} " + root + "/x",
	}
	for _, cmd := range cases {
		if !r.verdict(cmd).Deny {
			t.Errorf("worktree gate: %q allowed, want refused", cmd)
		}
		if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env); got.Surface == "" {
			t.Errorf("memory gate: %q is no attempt, want one", cmd)
		}
		if _, denied := githubPostJudgeText(cmd, cwd); !denied {
			t.Errorf("GitHub gate: %q allowed, want refused", cmd)
		}
	}
}

// TestBraceControlsAllowedInAllGates: quoted, escaped, lone-brace and parameter-expansion spellings are no brace expansion
// and stay ordinary words in every gate.
func TestBraceControlsAllowedInAllGates(t *testing.T) {
	r := newDelRig(t)
	cwd, _, env := gateScene(t)
	for _, cmd := range []string{"echo '{a,b}'", "echo \"{a,b}\"", "echo \\{a,b\\}", "echo {}", "echo ${HOME}"} {
		if v := r.verdict(cmd); v.Deny {
			t.Errorf("worktree gate: %q refused (%s), want allowed", cmd, v.Reason)
		}
		if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env); got.Surface != "" {
			t.Errorf("memory gate: %q is an attempt (%+v), want none", cmd, got)
		}
		if _, denied := githubPostJudgeText(cmd, cwd); denied {
			t.Errorf("GitHub gate: %q denied, want allowed", cmd)
		}
	}
}
