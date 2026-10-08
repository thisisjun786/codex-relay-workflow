package hook

import "testing"

// TestBraceWordsInTheWorktreeGate: a brace expansion that hides the removal verb or its target is refused in a managed
// worktree; the quoted, escaped and lone-brace spellings are ordinary words and stay allowed.
func TestBraceWordsInTheWorktreeGate(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"rm -rf " + r.checkout + "{,.bak}",
		"{rm,echo} -rf ../repo",
	} {
		if !r.verdict(cmd).Deny {
			t.Errorf("%q: allowed, want refused", cmd)
		}
	}
	for _, cmd := range []string{"echo '{a,b}'", "echo \\{a,b\\}", "echo {}"} {
		if v := r.verdict(cmd); v.Deny {
			t.Errorf("%q: refused (%s), want allowed", cmd, v.Reason)
		}
	}
}

// TestBraceWordsInTheMemoryGate: a brace expansion that hides the copy verb or the memory destination is an attempt to
// write the memory directory; the quoted spelling of the same text is no write.
func TestBraceWordsInTheMemoryGate(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, cmd := range []string{"{cp,install} /tmp/x " + root + "/a", "cp /tmp/x " + root + "/{a,b}"} {
		if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env); got.Surface == "" {
			t.Errorf("%q: no write attempt, want one", cmd)
		}
	}
	if got := memoryGateClassify("Bash", map[string]any{"command": "echo '{a,b}' > /tmp/out"}, cwd, env); got.Surface != "" {
		t.Errorf("a quoted brace text: %+v, want no attempt", got)
	}
}
