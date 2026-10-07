package hook

import (
	"strings"
	"testing"
)

// CRW-894: the residual forms of the program-read-from-a-pipe position that CRW-726's pipe rule left open — a
// standard-input alias that reopens the pipe, exec's -a option, a shell inside the -c program or the eval operand of a
// piped shell, busybox, and an interpreter that reads its program from standard input. CXC v0.2.40 allows every one of
// them, so each is a security fix (port: fixed). Every row was checked in bash 5.3.9 with a harmless stand-in for rm (a
// touch in a temporary directory); no row runs a deletion, and the guard reads text and runs nothing.

// TestWorktreeDelPipeStdinAliasDenied is c1's redirection half: a redirection on descriptor 0 whose target is
// /dev/stdin, /dev/fd/N, /proc/self/fd/N or /proc/<anything>/fd/N does not replace the pipe, because the guard cannot
// know what that descriptor holds (fail closed).
func TestWorktreeDelPipeStdinAliasDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash </dev/stdin",
		"printf 'rm -rf ../repo' | bash </dev/fd/0",
		"printf 'rm -rf ../repo' | bash 0</dev/fd/0",
		"printf 'rm -rf ../repo' | bash 0</dev/stdin",
		"printf 'rm -rf ../repo' | bash </proc/self/fd/0",
		"printf 'rm -rf ../repo' | bash 0</proc/self/fd/0",
		"printf 'rm -rf ../repo' | bash </proc/1/fd/0",
		"printf 'rm -rf ../repo' | bash </proc/self/fd/3",
		"printf 'rm -rf ../repo' | sh </dev/stdin",
		"printf 'rm -rf ../repo' | bash 2</dev/null", // descriptor 2 is not stdin: the shell still reads the pipe
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A file on descriptor 0 that is no alias still replaces the pipe.
	r.allowed(t,
		"printf 'echo hi' | bash </dev/null",
		"printf 'echo hi' | bash 0</dev/null",
		"printf 'echo hi' | bash < /dev/null",
	)
	r.intact(t)
}

// TestWorktreeDelPipeStdinAliasOperandDenied is c1's operand half: a script operand of a listed shell, or a source or .
// operand, that names one of those paths is a program the shell reads from the pipe.
func TestWorktreeDelPipeStdinAliasOperandDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash /dev/stdin",
		"printf 'rm -rf ../repo' | bash /proc/self/fd/0",
		"printf 'rm -rf ../repo' | sh /dev/fd/0",
		"printf 'rm -rf ../repo' | sh /proc/1/fd/0",
		"printf 'rm -rf ../repo' | source /dev/stdin",
		"printf 'rm -rf ../repo' | . /dev/stdin",
		"printf 'rm -rf ../repo' | . /dev/fd/0",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A script file, a source file and a shell that takes its program with -c stay as they are.
	r.allowed(t,
		"printf x | bash script.sh",
		"printf x | source ./env.sh",
		"printf x | . ./env.sh",
		"printf x | bash -c 'cat'",
	)
	r.intact(t)
}

// TestWorktreeDelPipeExecOptionDenied is c2: the shared wrapper table reads exec's -a NAME, so the pipe rule and the
// removal check both name the command behind it.
func TestWorktreeDelPipeExecOptionDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | exec -a x bash",
		"printf 'rm -rf ../repo' | exec -a x sh",
		"printf 'rm -rf ../repo' | exec -l bash",
		"printf 'rm -rf ../repo' | exec -c bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// The removal check reads the same table: exec -a NAME rm is a removal behind a wrapper.
	r.denied(t, "exec -a x rm -rf ../repo", "rm -r ../repo")
	r.allowed(t, "exec -a x rm -rf ../other", "exec -a x true", "printf x | exec -a x cat")
	r.intact(t)
}

// TestWorktreeDelPipeShellInsideProgramDenied is c3: when the right side of a pipe is a listed shell with a -c program,
// or eval, the shells inside that program or operand inherit the pipe, so one with no -c, no script operand and no file
// on its own descriptor 0 reads it. The nesting uses the reading-depth budget and fails closed at the cap.
func TestWorktreeDelPipeShellInsideProgramDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash -c 'bash'",
		"printf 'rm -rf ../repo' | sh -c 'exec bash'",
		"printf 'rm -rf ../repo' | bash -c 'nohup bash'",
		"printf 'rm -rf ../repo' | eval bash",
		"printf 'rm -rf ../repo' | eval 'bash'",
		"printf 'rm -rf ../repo' | bash -c 'bash -c bash'",
		"printf 'rm -rf ../repo' | bash -c 'cat | bash'",
		"printf 'rm -rf ../repo' | eval 'sh'",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A program that runs something other than a shell, a script operand and a -c program stay as they are.
	r.allowed(t,
		"printf x | bash -c 'cat'",
		"printf x | bash -c 'echo hi'",
		"printf x | eval echo ok",
		"printf x | eval 'cat'",
		"printf x | bash -c 'bash script.sh'",
	)
	r.intact(t)
}

// TestWorktreeDelPipeBusyboxDenied is c4's first half: busybox is a wrapper whose first operand names the applet, so
// busybox sh is a listed shell reading the pipe.
func TestWorktreeDelPipeBusyboxDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | busybox sh",
		"printf 'rm -rf ../repo' | busybox bash",
		"printf 'rm -rf ../repo' | busybox ash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// busybox names the applet, so its removal applet is a removal too, and an applet that is no shell reads the pipe
	// as its own input.
	r.denied(t, "busybox rm -rf ../repo", "rm -r ../repo")
	r.allowed(t, "busybox rm -rf ../other", "printf x | busybox cat")
	r.intact(t)
}

// TestWorktreeDelPipeInterpreterDenied is c4's second half: an interpreter called with no program argument reads its
// program from the pipe.
func TestWorktreeDelPipeInterpreterDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf x | python",
		"printf x | python3",
		"printf x | perl",
		"printf x | ruby",
		"printf x | node",
		"printf x | php",
		"printf x | python3 -",
		"printf x | python3 -O",
		"printf x | node --eval",
	} {
		w := "an interpreter program read from a pipe"
		if got := r.verdict(cmd); !got.Deny || !strings.Contains(got.Reason, w) {
			t.Errorf("%q: got %+v, want a deny naming %q", cmd, got, w)
		}
	}
	// A program argument (-c, -e, -r, -m) and a script operand are readable positions and stay allowed.
	r.allowed(t,
		"printf x | python3 -c 'print(1)'",
		"printf x | python3 -m json.tool",
		"printf x | perl -e 'print 1'",
		"printf x | ruby -e 'puts 1'",
		"printf x | node script.js",
		"printf x | php -r 'echo 1;'",
		"printf x | node -e 'console.log(1)'",
	)
	r.intact(t)
}

// TestWorktreeDelHereStringInterpreterDenied is c4's here-string half: an interpreter reads the here-string as its
// program whatever the word holds, so the word needs no expansion.
func TestWorktreeDelHereStringInterpreterDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"python3 <<< 'import os'",
		"python3 <<< \"import os\"",
		"perl <<< 'print 1'",
		"python3 <<< $'import os'",
	} {
		worktreeDelUnreadableDenied(t, r, cmd, "an interpreter program read from a here-string")
	}
	// An interpreter with a program argument, and a here-string that feeds something else, stay allowed.
	r.allowed(t,
		"python3 -c 'print(1)' <<< 'x'",
		"cat <<< 'import os'",
		"node script.js <<< 'x'",
	)
	r.intact(t)
}

// TestWorktreeDelHeredocInterpreterDenied is c4's here-document half: an interpreter with no program argument reads the
// here-document as its program.
func TestWorktreeDelHeredocInterpreterDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"python3 <<EOF\nimport os\nEOF",
		"python3 <<'EOF'\nimport os\nEOF",
		"perl <<EOF\nprint 1\nEOF",
		"python3 - <<EOF\nimport os\nEOF",
	} {
		worktreeDelUnreadableDenied(t, r, cmd, "an interpreter program read from a here-document")
	}
	// A here-document that feeds something else, and an interpreter with a program argument, stay allowed.
	r.allowed(t,
		"cat <<EOF\nimport os\nEOF",
		"python3 -c 'print(1)' <<EOF\nx\nEOF",
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualControls is c5's allowed list: the new rules only add denies.
func TestWorktreeDelPipeResidualControls(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"printf 'echo hi' | bash </dev/null",
		"printf x | (cd sub; cat)",
		"printf x | { cat; }",
		"printf x | nohup cat",
		"bash -c 'echo hi'",
		"exec -a x true",
		"busybox cat",
		"python3 script.py",
		"python3 -m json.tool < in.json",
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualNestingFailsClosed is c3's budget clause: a program handed on past the reading-depth limit
// is refused rather than allowed, whichever reading reaches it first.
func TestWorktreeDelPipeResidualNestingFailsClosed(t *testing.T) {
	r := newDelRig(t)
	program := "bash"
	for range worktreeDelQuoteDepth + 2 {
		program = "bash -c \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(program) + "\""
	}
	cmd := "printf 'rm -rf ../repo' | " + program
	if got := r.verdict(cmd); !got.Deny || !strings.Contains(got.Reason, "nested past the reading depth") {
		t.Errorf("the nest must fail closed: got %+v", got)
	}
	r.intact(t)
}
