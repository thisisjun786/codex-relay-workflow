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
		// CRW-894 c10: the forms the gate shares with the worktree guard. (a) another process's descriptor is
		// unknown and fails closed; (b) a redirection between exec's options and its executable hides the shell;
		// (c) a shell as the condition of a compound, and a negated shell, are commands the reading classifies;
		// (d) a here-string written before the command name belongs to that command; (e) the & of a descriptor
		// duplication is no command separator; (f) a -word is no argument of an unknown-arity option; (h) php's
		// -f/--file naming a standard-input alias and python's -m code read the pipe.
		"printf 'echo x > " + root + "/a' | bash -c 'bash </dev/null </proc/$$/fd/0; :'",
		"printf 'echo x > " + root + "/a' | exec -a x >/dev/null bash",
		"printf 'echo x > " + root + "/a' | bash -c 'if bash; then :; fi'",
		"printf 'echo x > " + root + "/a' | bash -c '! bash'",
		"<<< 'echo x > " + root + "/a' bash",
		"python3 2>&1 <<'PY'\nopen('" + root + "/a','w')\nPY",
		"printf x | node --no-warnings --require fs",
		"printf x | php -f /dev/stdin",
		"printf x | python3 -m code",
		// CRW-894 c10, third round: the forms the gate shares with the worktree guard. (a) an output-style
		// duplication of the saved pipe back onto descriptor 0, and a read-write reopen of an alias;
		// (b) a condition argument named then or do is no clause introducer; (d) a here-document whose owner
		// stands behind a condition prefix; (e) a here-document written on another descriptor and copied back
		// onto descriptor 0.
		"printf 'echo x > " + root + "/a' | bash -c 'bash 3<&0 </dev/null 0>&3'",
		"printf 'echo x > " + root + "/a' | bash -c 'bash <>/dev/stdin'",
		"printf 'echo x > " + root + "/a' | bash -c 'if bash -s then; then :; fi'",
		"printf 'echo x > " + root + "/a' | bash -c 'if ! bash -s then; then :; fi'",
		// CRW-894 c10, fourth round: (a) a lone - after -- is the standard-input operand; (b) a redirection
		// between exec's -a and its argument; (c) a quoted -c or eval program is text; (d) an interpreter's
		// script operand names a descriptor the command itself opened.
		"printf 'echo x > " + root + "/a' | python3 -- -",
		"printf 'echo x > " + root + "/a' | exec -a >/dev/null x bash",
		"printf 'echo x > " + root + "/a' | bash -c 'eval \"0</dev/null; bash\"'",
		"printf 'echo x > " + root + "/a' | python3 /dev/fd/3 3<&0 </dev/null",
		"if python3 <<'PY'\nopen('" + root + "/a','w')\nPY\nthen :; fi",
		"python3 3<<'PY' <&3\nopen('" + root + "/a','w')\nPY",
		// CRW-894 c10, fifth round: (a) a quoted argument that only looks like a redirection is the option
		// argument it is; (b) MULTIOS reaches the descriptor that holds the pipe; (c) a here-document after a
		// compound closer feeds the whole compound; (d) a condition prefix before a here-string does not hide
		// the interpreter owner.
		"printf 'echo x > " + root + "/a' | env -u '>/dev/null' bash",
		"printf 'echo x > " + root + "/a' | python3 /dev/fd/3 3<&0 3</dev/null",
		"while python3; do :; done <<'PYEOF'\nopen('" + root + "/a','w')\nPYEOF",
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
		"printf x | node --no-warnings script.js",
		"python3 3<<EOF\nignored\nEOF",
	} {
		if got := memoryGateClassify("Bash", map[string]any{"command": command}, cwd, env); got.Surface != "" {
			t.Errorf("%q: %+v, want no write attempt", command, got)
		}
	}
}
