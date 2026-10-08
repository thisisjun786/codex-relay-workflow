package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-727: the memory write gate fails closed on a shell program it cannot read. When ShellWriteDestinations names no
// destination inside the protected root and worktreeDelUnreadableProgram (CRW-726) answers ok, the call is a write
// attempt of its own, beside CRW-741's f-string check. CXC v0.2.40 returns no attempt for these forms: this is a
// security fix (port: fixed).

// TestMemoryGateUnreadableProgramClassify is c10 and c12: the classification, the reason shape and the rows that stay
// no attempt.
func TestMemoryGateUnreadableProgramClassify(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command, what string }{
		{"bash -c", "bash -c \"$(printf 'echo x > " + root + "/a')\"", "a bash -c program"},
		{"pipe", "printf 'echo x > " + root + "/a' | bash", "a shell program read from a pipe"},
		{"eval", "eval \"$X\"", "an eval operand"},
		{"heredoc", "bash <<EOF\n$(printf 'echo x > " + root + "/a')\nEOF", "a shell program read from a here-document"},
	} {
		t.Run(c.name, func(t *testing.T) {
			want := "(a program the gate cannot read: " + c.what + ")"
			got := memoryGateClassify("Bash", map[string]any{"command": c.command}, cwd, env)
			if got.Surface != "shell" || !strings.HasPrefix(got.Target, "(a program the gate cannot read: ") {
				t.Errorf("%q: %+v, want the shell surface and %s", c.command, got, want)
			}
		})
	}
	for _, command := range []string{
		"bash -c 'echo hi'",
		"echo \"$HOME\"",
		"ls | grep x",
		"echo hi > /w/out.txt",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%q must be no attempt: %+v", command, got)
		}
	}
	// The check sits behind the protected-root guard, so a gate with no resolvable root makes no attempt.
	if _, ok := (memoryGateEnv{homeOK: false}).root(); ok {
		t.Error("a gate env with no resolvable home must have no protected root")
	}
}

// TestMemoryGateUnreadableProgramDeniesAndSpends is c12's envelope case: an unreadable program is denied without a
// grant, and with one the write passes and the grant is consumed.
func TestMemoryGateUnreadableProgramDeniesAndSpends(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, c := range []struct{ name, command string }{
		{"bash -c", "bash -c \"$(printf 'echo x > " + root + "/a')\""},
		{"pipe", "printf 'echo x > " + root + "/a' | bash"},
		{"eval", "eval \"$X\""},
	} {
		t.Run(c.name, func(t *testing.T) {
			payload := gateBash(t, cwd, c.command)
			reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
			if !strings.Contains(reason, "a program the gate cannot read: ") {
				t.Errorf("the reason does not name the unreadable program: %s", reason)
			}
			gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
			if out := HandleMemoryWriteGate(payload, env); out != "" {
				t.Errorf("with a grant the write must pass: %s", out)
			}
			if out := HandleMemoryWriteGate(payload, env); !strings.Contains(out, "MEMORY-WRITE-GATE") {
				t.Errorf("the grant must be spent after one write: %q", out)
			}
		})
	}
}
