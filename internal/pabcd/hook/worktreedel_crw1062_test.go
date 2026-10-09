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

// TestCRW1062ResidualBypassesAreRefused (CRW-1062, pre-merge findings d1 to d4): a move or a Git removal the guard passed
// because a nested shell, an unrelated directory, POSIX option parsing or a Git global option hid the checkout. The
// controls next to them stay allowed.
func TestCRW1062ResidualBypassesAreRefused(t *testing.T) {
	r := newDelRig(t)
	r.denied(t,
		// d1: find -exec runs sh -c; the {} placeholder is the checkout at run time.
		"find ../repo -maxdepth 0 -exec sh -c 'mv {} /tmp/gone' \\;",
		"find ../repo -maxdepth 0 -exec sh -c 'git worktree remove -f {}' \\;",
		// d2: the source is unknown from an unrelated directory; it may name the checkout through a variable.
		"cd /tmp && mv \"$SRC\" /tmp/gone",
		// d3: with POSIXLY_CORRECT, -S is an operand and ../repo is a source.
		"POSIXLY_CORRECT=1 mv build.log -S ../repo /tmp/gone",
		// d4: Git global options before the subcommand.
		"git -P worktree remove -f ../repo",
		"git --no-pager worktree remove -f ../repo",
		"printf '../%s\\n' repo | xargs git -P worktree remove -f",
	)
	r.allowed(t,
		"find . -maxdepth 0 -exec sh -c 'echo {}' \\;",
		"mv build.log \"$DEST\"",
		"POSIXLY_CORRECT=1 mv build.log archive/",
		"POSIXLY_CORRECT=1 mv -t archive/ build.log",
		"git -P status",
		"git --no-pager worktree list",
	)
}
