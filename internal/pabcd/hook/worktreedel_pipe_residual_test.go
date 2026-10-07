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

// worktreeDelPipeInterpreterDenied asserts a deny of the interpreter pipe position.
func worktreeDelPipeInterpreterDenied(t *testing.T, r delRig, cmd string) {
	t.Helper()
	worktreeDelUnreadableDenied(t, r, cmd, "an interpreter program read from a pipe")
}

// TestWorktreeDelPipeStdinAliasDenied is c1's redirection half: a redirection on descriptor 0 whose target is
// /dev/stdin, /dev/fd/N, /proc/self/fd/N or /proc/<anything>/fd/N does not replace the pipe, because the guard cannot
// know what that descriptor holds (fail closed). The path is read as the kernel resolves it, so a lexical `.` or a
// repeated separator is dropped first.
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
		"printf 'rm -rf ../repo' | bash </dev/./stdin",
		"printf 'rm -rf ../repo' | bash </dev//stdin",
		"printf 'rm -rf ../repo' | bash </proc/self/../self/fd/0",
		"printf 'rm -rf ../repo' | bash 2</dev/null", // descriptor 2 is not stdin: the shell still reads the pipe
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A file on descriptor 0 that is no alias still replaces the pipe, in any order: an alias reopens the descriptor as
	// it already stands, so `bash </dev/null </dev/stdin` still reads /dev/null (checked in bash 5.3.9).
	r.allowed(t,
		"printf 'echo hi' | bash </dev/null",
		"printf 'echo hi' | bash 0</dev/null",
		"printf 'echo hi' | bash < /dev/null",
		"printf 'echo hi' | bash </dev/stdin </dev/null",
		"printf 'echo hi' | bash </dev/null </dev/stdin",
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
		"printf 'rm -rf ../repo' | bash /dev/./stdin",
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
// busybox sh is a listed shell reading the pipe. A busybox global mode dispatches no applet, so the word after it is no
// command.
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
	// A global mode takes no applet: --help, --list, --list-full, --install and --show.
	r.allowed(t, "busybox --help rm -rf ../repo", "busybox --list", "busybox --list-full", "busybox --install -s ../repo", "busybox --show rm -rf ../repo")
	r.intact(t)
}

// TestWorktreeDelPipeInterpreterDenied is c4's second half: an interpreter called with no program argument reads its
// program from the pipe. The option parse skips the argument of an option that takes one (python3 -W ignore) and reads
// the versioned and alternate names the repository already knows (python3.11, py, nodejs), and a script operand that
// names the process's own standard input is a program read from the pipe.
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
		"printf x | python3 -W ignore",
		"printf x | python3 -X dev",
		"printf x | python3 -Wignore",
		"printf x | node -r module",
		"printf x | node --require module",
		"printf x | python3.11",
		"printf x | python2",
		"printf x | py",
		"printf x | nodejs",
		"printf x | python3 /dev/stdin",
		"printf x | python3 /dev/fd/0",
	} {
		worktreeDelPipeInterpreterDenied(t, r, cmd)
	}
	// A program argument (-c, -e, -r, -m), a script operand and a file on descriptor 0 are readable positions and stay
	// allowed.
	r.allowed(t,
		"printf x | python3 -c 'print(1)'",
		"printf x | python3 -m json.tool",
		"printf x | perl -e 'print 1'",
		"printf x | ruby -e 'puts 1'",
		"printf x | node script.js",
		"printf x | node -e 'console.log(1)'",
		"printf x | node --eval 'console.log(1)'",
		"printf x | php -r 'echo 1;'",
		"printf x | python3 </dev/null",
		"printf x | python3 -c 'print(1)' -",
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

// TestWorktreeDelHeredocInterpreterDenied is c4's here-document half: an interpreter with no program argument reads a
// here-document on its standard input as its program, in both the attached (<<EOF) and the spaced (<< EOF) form. A
// descriptor other than 0 feeds another descriptor, so it is no program: python3 3<<EOF still reads standard input.
func TestWorktreeDelHeredocInterpreterDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"python3 <<EOF\nimport os\nEOF",
		"python3 <<'EOF'\nimport os\nEOF",
		"perl <<EOF\nprint 1\nEOF",
		"python3 - <<EOF\nimport os\nEOF",
		"python3 << EOF\nimport os\nEOF",
		"python3 <<- EOF\n\timport os\n\tEOF",
		"python3 0<< EOF\nimport os\nEOF",
		"python3.11 << EOF\nimport os\nEOF",
	} {
		worktreeDelUnreadableDenied(t, r, cmd, "an interpreter program read from a here-document")
	}
	// A here-document that feeds something else, an interpreter with a program argument, and a here-document on another
	// descriptor stay allowed.
	r.allowed(t,
		"cat <<EOF\nimport os\nEOF",
		"cat << EOF\nimport os\nEOF",
		"python3 -c 'print(1)' <<EOF\nx\nEOF",
		"python3 3<<EOF\nignored\nEOF",
		"python3 3<< EOF\nignored\nEOF",
		"python3 2<<EOF\nignored\nEOF",
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
// is refused rather than allowed, whichever reading reaches it first (the pipe rule's own bound, or the walk's depth
// rule, which reaches the same nest through the segment after the pipe). The row pins that the answer is a refusal and
// that the recursion terminates; it does not separate the two readings.
func TestWorktreeDelPipeResidualNestingFailsClosed(t *testing.T) {
	r := newDelRig(t)
	program := "bash"
	for range worktreeDelQuoteDepth + 2 {
		program = "bash -c \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(program) + "\""
	}
	cmd := "printf 'rm -rf ../repo' | " + program
	if got := r.verdict(cmd); !got.Deny {
		t.Errorf("the nest must fail closed: got %+v", got)
	} else if !strings.Contains(got.Reason, "nested past the reading depth") {
		t.Logf("the refusal names another rule: %s", got.Reason)
	}
	r.intact(t)
}

// TestWorktreeDelPipeResidualReviewFindings pins the independent review's findings on the first head of this change.
// Each was a fail-open of the new rule, except the last two, which were over-denials the review also found.
func TestWorktreeDelPipeResidualReviewFindings(t *testing.T) {
	r := newDelRig(t)
	// A lone - is the interpreter's program operand: the words after it are its own arguments, so the interpreter still
	// reads the pipe (bash 5.3.9 and python3 3.14 run the piped program).
	for _, cmd := range []string{
		"printf x | python3 - ignored.py",
		"printf x | python3 - -c 'print(0)'",
		"printf x | python3 -O - -c 'print(0)'",
	} {
		worktreeDelPipeInterpreterDenied(t, r, cmd)
	}
	// exec's argument-taking letter may stand inside a cluster of flags.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | exec -ca x bash",
		"printf 'rm -rf ../repo' | exec -la x bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// The last redirection that names descriptor 0 decides what it holds, and a duplication the guard cannot follow
	// leaves the pipe: python3 3<&0 </dev/null <&3 runs the piped program in bash 5.3.9.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 3<&0 </dev/null <&3")
	// A long option whose arity the guard does not know is read as taking the next word (python3
	// --check-hash-based-pycs default runs the piped program), so the interpreter is left with no program argument.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 --check-hash-based-pycs default")
	// A -c program that can name its operands runs them as a command line of its own, and that line inherits the pipe:
	// bash -c 'exec \"$@\"' _ bash runs bash with the pipe on its standard input.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'exec \"$@\"' _ bash")
	// A redirection written before the command word counts too: 0</dev/null python3 reads /dev/null and no program runs.
	r.allowed(t,
		"printf x | 0</dev/null python3",
		"printf x | bash </dev/null <&0", // <&0 duplicates descriptor 0, which already holds the file
		"printf x | python3 </dev/null <&0",
	)
	r.intact(t)
}
