package hook

import "testing"

// TestMoveOfCheckoutOrAncestorIsRefused (CRW-1062): a move takes the directory it names away from the session, as rm -r
// does, so moving the managed checkout or one of its ancestors is refused. A move of anything inside the worktree is a control.
func TestMoveOfCheckoutOrAncestorIsRefused(t *testing.T) {
	r := newDelRig(t)
	r.denied(t,
		"mv ../repo /tmp/gone",
		"mv -- ../repo /tmp/gone",
		"mv .. /tmp/gone",
		"mv -t /tmp/gone ../repo",
		"mv --target-directory=/tmp/gone ../repo",
		"mv -v ../repo /tmp/gone",
	)
	r.allowed(t,
		"mv ./build ./out",
		"mv build.log archive/",
		"mv ../repo/build /tmp/x",
		"mv -t archive/ build.log",
	)
}

// TestMoveWithUnknownOperandIsRefused (CRW-1062): a move whose source the reader cannot read might move the checkout.
func TestMoveWithUnknownOperandIsRefused(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "mv \"$SRC\" /tmp/x")
}

// TestXargsFedRemovalIsRefused (CRW-1062): the operands xargs reads from its input arrive at run time. The reader does not
// prove what the input holds, so a removal or move behind xargs is refused whatever the input is. The printf format, the file
// operand filter, the custom delimiter and a plain echo are all refused; an unrelated xargs is a control.
func TestXargsFedRemovalIsRefused(t *testing.T) {
	r := newDelRig(t)
	r.denied(t,
		"printf '../%s\\n' repo | xargs git worktree remove -f",
		"echo x | cat list.txt | xargs git worktree remove -f",
		"echo ../repo,x | xargs -d, git worktree remove -f",
		"echo ../repo | xargs git worktree remove -f",
		"echo ../repo | xargs mv -t /tmp/gone",
	)
	r.allowed(t,
		"echo x | xargs wc -l",
		"printf '%s\\n' a.txt | xargs cat",
		"echo x | xargs git status",
		"xargs rm ./build",
	)
}

// TestCRW1062EntryPointRefusesTheNamedShapes: the four shapes the issue names are refused at the PreToolUse entry point, and the
// two controls are allowed there.
func TestCRW1062EntryPointRefusesTheNamedShapes(t *testing.T) {
	rig := newDelRig(t)
	for _, command := range []string{
		"mv ../repo /tmp/gone",
		"printf '../%s\\n' repo | xargs git worktree remove -f",
		"echo x | cat list.txt | xargs git worktree remove -f",
		"echo ../repo,x | xargs -d, git worktree remove -f",
	} {
		raw := gatePayload(t, rig.checkout, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}})
		if HandleWorktreeGuardPreTool(raw, rig.env()) == "" {
			t.Errorf("%q: the entry point passed it; want a deny", command)
		}
	}
	for _, command := range []string{"mv ./build ./out", "echo x | xargs wc -l"} {
		raw := gatePayload(t, rig.checkout, map[string]any{"tool_name": "Bash", "tool_input": map[string]any{"command": command}})
		if got := HandleWorktreeGuardPreTool(raw, rig.env()); got != "" {
			t.Errorf("%q: the entry point denied a control: %s", command, got)
		}
	}
}
