package hook

import (
	"strings"
	"testing"
)

// runnerRows are the program-running wrappers of the ruling (watch, flock, chroot, script, strace, ltrace, entr, at, batch),
// each with the form that makes its program a shell string, an argv, a file operand or a stdin program.
func runnerRows(root string) []string {
	return []string{
		"watch -n 1 cp /tmp/x " + root + "/a",
		"watch cp /tmp/x " + root + "/a",
		"flock /tmp/lock cp /tmp/x " + root + "/a",
		"flock -c 'cp /tmp/x " + root + "/a' /tmp/lock",
		"flock /tmp/lock -c 'cp /tmp/x " + root + "/a'",
		"flock -w 1 /tmp/lock -c 'cp /tmp/x " + root + "/a'",
		"chroot /tmp/newroot cp /tmp/x " + root + "/a",
		"script -q -c 'cp /tmp/x " + root + "/a' /dev/null",
		"script -a -c 'echo hi' " + root + "/log",
		"strace -o " + root + "/trace cp /tmp/x " + root + "/a",
		"ltrace -o " + root + "/trace cp /tmp/x " + root + "/a",
		"entr -s 'cp /tmp/x " + root + "/a'",
	}
}

// TestRunnerWrappersNameTheirDestination: each runner's inner write or transcript file is a named memory destination.
func TestRunnerWrappersNameTheirDestination(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, cmd := range runnerRows(root) {
		got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env)
		if got.Surface != "shell" || !strings.HasPrefix(got.Target, root) {
			t.Errorf("%q: %+v, want the memory destination named", cmd, got)
		}
	}
}

// TestRunnerWrappersUnprovableAreAttempts: a runner whose program the text does not show, or whose operands arrive at run
// time, is a write attempt of its own in the memory gate, not a silent allow.
func TestRunnerWrappersUnprovableAreAttempts(t *testing.T) {
	cwd, _, env := gateScene(t)
	for _, cmd := range []string{
		"at now + 1 minute",
		"batch",
		"script -q /dev/null",
		"chroot /tmp/newroot",
		"printf '%s' " + "/tmp/x" + " | entr cp /tmp/x",
	} {
		got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env)
		if got.Surface == "" {
			t.Errorf("%q: no attempt, want the gate to ask", cmd)
		}
	}
}

// TestRunnerWrappersRemovalDeniedInWorktree: each runner's inner removal is denied in a managed worktree, and the same
// runner aimed at the unrelated neighbour directory is allowed.
func TestRunnerWrappersRemovalDeniedInWorktree(t *testing.T) {
	r := newDelRig(t)
	for _, tmpl := range []string{
		"watch -n 1 rm -rf {CHECKOUT}",
		"watch rm -rf {CHECKOUT}",
		"flock /tmp/lock rm -rf {CHECKOUT}",
		"flock -c 'rm -rf {CHECKOUT}' /tmp/lock",
		"chroot /tmp/newroot rm -rf {CHECKOUT}",
		"script -q -c 'rm -rf {CHECKOUT}' /dev/null",
		"strace -o /tmp/t rm -rf {CHECKOUT}",
		"ltrace rm -rf {CHECKOUT}",
		"printf '%s' {CHECKOUT} | entr rm -rf",
		"entr -s 'rm -rf {CHECKOUT}'",
		"echo 'rm -rf {CHECKOUT}' | at now",
	} {
		cmd := strings.ReplaceAll(tmpl, "{CHECKOUT}", r.checkout)
		if !r.verdict(cmd).Deny {
			t.Errorf("%q: allowed in a managed worktree, want refused", cmd)
		}
		control := strings.ReplaceAll(tmpl, "{CHECKOUT}", r.other)
		// A run-time operand (entr, xargs) is refused for any target, because the reader cannot see its stdin: no control.
		if v := r.verdict(control); v.Deny && !strings.Contains(tmpl, "at now") && !strings.Contains(tmpl, "entr rm") {
			t.Errorf("control %q: refused (%s), want allowed", control, v.Reason)
		}
	}
}

// TestRunnerWrappersBenignAllowed: the runners with a benign program stay allowed in the worktree gate and make no
// memory attempt.
func TestRunnerWrappersBenignAllowed(t *testing.T) {
	r := newDelRig(t)
	cwd, _, env := gateScene(t)
	for _, cmd := range []string{
		"watch -n 1 echo hi",
		"flock /tmp/lock echo hi",
		"chroot /tmp/newroot ls",
		"script -q -c 'echo hi' /tmp/transcript",
		"strace -o /tmp/t ls",
		"printf '%s' x | entr -s 'echo hi'",
	} {
		if v := r.verdict(cmd); v.Deny {
			t.Errorf("%q: refused in the worktree gate (%s), want allowed", cmd, v.Reason)
		}
		if got := memoryGateClassify("Bash", map[string]any{"command": cmd}, cwd, env); got.Surface != "" {
			t.Errorf("%q: memory attempt %+v, want none", cmd, got)
		}
	}
}
