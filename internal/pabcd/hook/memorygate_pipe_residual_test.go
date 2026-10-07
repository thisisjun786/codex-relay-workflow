package hook

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-894: the memory gate reads the same discriminator as the worktree guard (worktreeDelUnreadableProgram), so the
// residual pipe forms CRW-726's pipe rule left open are write attempts of their own and the gate fails closed on them.
// The rows live in this file because the issue's coordination keeps the memory-gate rows here; memorygate.go is not
// edited (CRW-815 owns it).
func TestMemoryGatePipeResidual(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, command := range []string{
		"printf 'echo x > " + root + "/a' | exec -a x bash",
		"printf 'echo x > " + root + "/a' | bash </dev/stdin",
		"printf 'echo x > " + root + "/a' | bash /dev/stdin",
		"printf 'echo x > " + root + "/a' | bash -c 'bash'",
		"printf 'echo x > " + root + "/a' | busybox sh",
		"printf 'echo x > " + root + "/a' | python3",
		// CRW-894 c9(a): a descriptor that holds the pipe and is put back on descriptor 0.
		"printf 'echo x > " + root + "/a' | bash 3</dev/fd/0 </dev/null <&3",
		"printf 'echo x > " + root + "/a' | bash 3</dev/stdin </dev/null <&3",
		"printf 'echo x > " + root + "/a' | python3 3</dev/fd/0 </dev/null <&3",
		// CRW-894 c9(b), zsh MULTIOS: a descriptor-0 file does not replace the pipe.
		"printf 'echo x > " + root + "/a' | bash </dev/null",
		"printf 'echo x > " + root + "/a' | python3 </dev/null",
	} {
		got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env)
		if got.Surface != "shell" || !strings.HasPrefix(got.Target, "(a program the gate cannot read: ") {
			t.Errorf("%q: %+v, want a shell attempt whose target names the unreadable program", command, got)
		}
		payload := gateBash(t, cwd, command)
		reason := gateDeny(t, HandleMemoryWriteGate(payload, env))
		if !strings.Contains(reason, "(a program the gate cannot read: ") {
			t.Errorf("the reason does not name the unreadable program: %s", reason)
		}
		gateSeed(t, cwd, func(s *state.State) { s.MemoryWriteGrant = true })
		if out := HandleMemoryWriteGate(payload, env); out != "" {
			t.Errorf("with a grant the write must pass: %s", out)
		}
	}
	// A program the gate can read, and a pipe that feeds no program position, stay allowed.
	for _, command := range []string{
		"printf x | python3 -c 'print(1)'",
		"printf x | bash -c 'cat'",
		"printf x | cat",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%q: %+v, want no write attempt", command, got)
		}
	}
}
