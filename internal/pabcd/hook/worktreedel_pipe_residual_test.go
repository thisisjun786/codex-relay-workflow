package hook

import (
	"strings"
	"testing"
	"time"
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
	// Superseded by CRW-894 c9(b), zsh MULTIOS: the commands run through the user's shell, and zsh's multios option
	// is on by default on this host and on macOS, so a command on the right of a pipe is fed both the pipe and its
	// own descriptor-0 redirection and reads the pipe whatever that redirection says (zsh 5.9 runs
	// `printf 'echo X' | bash </dev/null`; bash 5.3.9 runs nothing). CRW-726 c15(a) allowed these rows; they deny now.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash </dev/null",
		"printf 'rm -rf ../repo' | bash 0</dev/null",
		"printf 'rm -rf ../repo' | bash < /dev/null",
		"printf 'rm -rf ../repo' | bash </dev/stdin </dev/null",
		"printf 'rm -rf ../repo' | bash </dev/null </dev/stdin",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
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
		"printf x | python3 -c 'print(1)' -",
	)
	// CRW-894 c9(b), zsh MULTIOS: an interpreter with no program argument reads the pipe whatever its own
	// descriptor-0 redirection says.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 </dev/null")
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
	// CRW-894 c9(b), zsh MULTIOS: a descriptor-0 redirection, wherever it is written and however it is spelled,
	// does not replace the pipe for a command on the right of one.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | 0</dev/null python3")
	worktreeDelPipeDenied(t, r, "printf x | bash </dev/null <&0")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 </dev/null <&0")
	r.intact(t)
}

// TestWorktreeDelPipeResidualC9 is CRW-894 c9, the generation-2 criterion, red first on head 46bcccf. Three parts:
// (a) the descriptor bookkeeping follows every descriptor, so a descriptor that holds the pipe and is put back on
// descriptor 0 is a program the guard cannot read; (b) zsh MULTIOS, on by default on this host and on macOS, feeds a
// command on the right of a pipe both the pipe and its own descriptor-0 redirection, so a descriptor-0 file does not
// replace the pipe; (c) ksh and mksh are listed shells for the -c, exec "$@" and stdin readings. Every row was
// checked in zsh 5.9 with a harmless stand-in for rm; the commands run through the user's shell, and zsh is that
// shell here, so these are the answers a real run gives.
func TestWorktreeDelPipeResidualC9(t *testing.T) {
	r := newDelRig(t)
	// (a) a descriptor that holds the pipe, put back on descriptor 0.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash 3</dev/fd/0 </dev/null <&3",
		"printf 'rm -rf ../repo' | bash 3</dev/stdin </dev/null <&3",
		"printf 'rm -rf ../repo' | bash 3</proc/self/fd/0 </dev/null <&3",
		"printf 'rm -rf ../repo' | bash 3<&0 </dev/null <&3",
		"printf 'rm -rf ../repo' | bash 4</dev/fd/0 3<&4 </dev/null <&3",
		"printf 'rm -rf ../repo' | bash 9</dev/fd/0 </dev/null <&9",
		"printf 'rm -rf ../repo' | bash </dev/null 3</dev/fd/0 <&3",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	worktreeDelPipeInterpreterDenied(t, r, "printf 'rm -rf ../repo' | python3 3</dev/fd/0 </dev/null <&3")
	worktreeDelPipeInterpreterDenied(t, r, "printf 'rm -rf ../repo' | python3 3</dev/stdin </dev/null <&3")
	worktreeDelPipeInterpreterDenied(t, r, "printf 'rm -rf ../repo' | python3 3<&0 </dev/null <&3")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | ksh 3</dev/fd/0 </dev/null <&3")
	// (b) zsh MULTIOS: a descriptor-0 file does not replace the pipe.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash </dev/null",
		"printf 'rm -rf ../repo' | bash 0</dev/null",
		"printf 'rm -rf ../repo' | 0</dev/null bash",
		"printf 'rm -rf ../repo' | bash - 0</dev/null",
		"printf 'rm -rf ../repo' | bash < /dev/null",
		"printf 'rm -rf ../repo' | bash 2</dev/null 0</dev/null",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	worktreeDelPipeInterpreterDenied(t, r, "printf 'rm -rf ../repo' | python3 </dev/null")
	// (c) ksh and mksh join the listed shells for the -c, exec "$@" and stdin readings.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | ksh",
		"printf 'rm -rf ../repo' | mksh",
		"printf 'rm -rf ../repo' | ksh -c 'bash'",
		"printf 'rm -rf ../repo' | mksh -c 'bash'",
		"printf 'rm -rf ../repo' | ksh /dev/stdin",
		"printf 'rm -rf ../repo' | mksh /dev/stdin",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | ksh -c 'exec \"$@\"' _ bash",
		"printf 'rm -rf ../repo' | mksh -c 'exec \"$@\"' _ bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A command with no pipe in front keeps its answers: MULTIOS is the pipe's rule, not the redirection's.
	r.allowed(t,
		"bash </dev/null",
		"bash <<< x",
		"bash 3</dev/null",
		"python3 </dev/null",
		"python3 -c 'print(1)' </dev/null",
		"ksh -c 'echo ok'",
		"mksh -c 'echo ok'",
	)
	// The controls the criterion names keep their answers.
	r.allowed(t,
		"printf x | (cd sub; cat)",
		"printf x | nohup cat",
		"printf x | python3 -c 'print(1)'",
		"printf x | bash -c 'cat'",
	)
	r.intact(t)
}

// TestWorktreeDelUnreadableStdinHoldings pins the descriptor walk CRW-894 c9(a) asks for: a redirection that opens a
// descriptor from an alias of the current standard input makes that descriptor hold the pipe, a duplication copies the
// descriptor it names, a plain file opens a file, and a descriptor the guard cannot follow is unknown. multios is off
// here, so a redirection on descriptor 0 applies as written; the pipe rule passes it on only for the simple command
// the user's shell feeds directly (CRW-894 c9(b)).
func TestWorktreeDelUnreadableStdinHoldings(t *testing.T) {
	pipe, file, unknown := worktreeDelStdinPipe, worktreeDelStdinFile, worktreeDelStdinUnknown
	for _, c := range []struct {
		cmd     string
		multios bool
		want    map[int]int
	}{
		{"bash 3</dev/fd/0 </dev/null <&3", false, map[int]int{0: pipe, 3: pipe}},
		// <&3 puts the pipe back on descriptor 0, so it holds the pipe again.
		{"bash 3</dev/fd/0 </dev/null <&3", false, map[int]int{0: pipe, 3: pipe}},
		{"bash 3</dev/fd/0 </dev/null", false, map[int]int{0: file, 3: pipe}},
		{"bash 3</dev/stdin", false, map[int]int{0: pipe, 3: pipe}},
		{"bash 3</proc/self/fd/0", false, map[int]int{0: pipe, 3: pipe}},
		{"bash 3<&0", false, map[int]int{0: pipe, 3: pipe}},
		{"bash 4</dev/fd/0 3<&4", false, map[int]int{0: pipe, 3: pipe, 4: pipe}},
		{"bash 3</dev/null", false, map[int]int{0: pipe, 3: file}},
		{"bash 3<&2", false, map[int]int{0: pipe, 3: unknown}},
		{"bash 3<&-", false, map[int]int{0: pipe, 3: unknown}},
		{"bash 2</dev/null", false, map[int]int{0: pipe, 2: file}},
		// MULTIOS: descriptor 0 keeps the pipe whatever is written on it.
		{"bash </dev/null", true, map[int]int{0: pipe}},
		{"bash 3</dev/fd/0 </dev/null <&3", true, map[int]int{0: pipe, 3: pipe}},
	} {
		got := worktreeDelUnreadableStdinHoldings(worktreeDelUnreadablePlainTexts(worktreeDelUnreadableWords(c.cmd)), c.multios)
		if len(got) != len(c.want) {
			t.Errorf("%q: %v, want %v", c.cmd, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("%q: descriptor %d = %d, want %d (%v)", c.cmd, k, got[k], v, got)
			}
		}
	}
}

// TestWorktreeDelPipeResidualMultiosScope pins where zsh's multios option reaches (CRW-894 c9(b), generation-2 review).
// The option belongs to the user's shell, so it applies to the simple command the shell feeds directly: there a
// descriptor-0 file does not replace the pipe. It does not reach a subshell, a brace group, a shell compound or a
// program handed to -c or eval, where the redirection is performed by the subshell or by the inner shell. zsh 5.9
// runs `printf 'echo X' | bash </dev/null` and prints nothing for the other four forms.
func TestWorktreeDelPipeResidualMultiosScope(t *testing.T) {
	r := newDelRig(t)
	// Direct: multios, so the descriptor-0 file does not replace the pipe.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash </dev/null")
	// Not reached: the file does replace the pipe, so nothing runs it.
	r.allowed(t,
		"printf x | (bash </dev/null)",
		"printf x | { bash </dev/null; }",
		"printf x | if true; then bash </dev/null; fi",
		"printf x | bash -c 'bash </dev/null'",
		"printf x | (python3 </dev/null)",
		"printf x | bash -c 'python3 </dev/null'",
	)
	// The outer redirection is still the user's shell's, so the pipe reaches the -c program.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'bash' </dev/null")
	r.intact(t)
}

// TestWorktreeDelPipeResidualGeneration2Findings pins the independent review of head 97f78bb2e: three fail-opens and
// one over-denial, each verified in zsh 5.9.
func TestWorktreeDelPipeResidualGeneration2Findings(t *testing.T) {
	r := newDelRig(t)
	// An interpreter option that takes an argument inside a cluster: python3 -OW ignore reads the pipe.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -OW ignore")
	// The last letter of the cluster takes the next word, so -WO ignore reads a script named ignore, and
	// -WOignore carries its argument attached; both forms leave the interpreter with no script operand of its
	// own, so the guard still reads the pipe there (fail closed; python3 -WOignore prints the piped program).
	r.allowed(t, "printf x | python3 -WO ignore")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -WOignore")
	// A -c program that names $0 runs it: bash -c 'exec \"$0\"' bash runs bash with the pipe.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'exec \"$0\"' bash")
	// A source operand behind a redirection is still the source's operand.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | source 2>/dev/null /dev/stdin")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | . 2>/dev/null /dev/stdin")
	// The same source form with no redirection, and a source file, stay as they were.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | source /dev/stdin")
	r.allowed(t, "printf x | source ./env.sh", "printf x | bash script.sh")
	r.intact(t)
}

// TestWorktreeDelUnreadableStdinHoldingsBound pins the two bounds the generation-2 review asked about: the descriptor
// walk saturates instead of overflowing (a run of digits longer than a descriptor can be names no descriptor, so the
// caller fails closed), and the pipe rule's recursion is bounded by the reading-depth budget rather than by the input.
func TestWorktreeDelUnreadableStdinHoldingsBound(t *testing.T) {
	r := newDelRig(t)
	// A descriptor number too large to be one is no descriptor the guard can follow: both forms fail closed.
	big := strings.Repeat("9", 30)
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash 3<&"+big+" <&3")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash "+big+"</dev/fd/0 <&"+big)
	// The recursion is bounded: a nest deeper than the reading-depth budget is refused and returns at once, whatever
	// the program names, so its cost does not grow with the depth.
	program := "true"
	for range worktreeDelQuoteDepth + 4 {
		program = "bash -c '" + program + " \"$@\" \"$0\"' _ bash"
	}
	cmd := "printf 'rm -rf ../repo' | " + program
	if got, ok := worktreeDelVerdictWithin(t, r, cmd, 10*time.Second); !ok {
		t.Fatal("the nest did not return within 10s")
	} else if !got.Deny {
		t.Errorf("the nest must fail closed: %+v", got)
	}
	r.intact(t)
}

// TestWorktreeDelPipeResidualC10 is CRW-894 c10, the generation-3 criterion, red first on head f54cf198. Each form was
// reproduced with a harmless stand-in for rm in the host's zsh (the shell the commands run through): (a) a
// /proc/<pid>/fd/N alias whose pid is not this process names a descriptor the guard cannot track; (b) a redirection
// written between a wrapper's options and its executable is skipped by the shared prefix walk; (c) a listed shell that
// stands as the condition of if, elif, while or until, or after !, is a command the pipe reading classifies; (d) a
// redirection written before the command name belongs to that command; (e) the & of a descriptor duplication is no
// command separator when the owner of a here-document is found; (f) a word that starts with - is never the argument of an
// option whose arity the guard does not know; (g) the -c and eval recursion carries the standard input the guard
// computed for the intermediate shell; (h) php -f/--file naming a standard-input alias and python -m code are programs
// read from the pipe.
func TestWorktreeDelPipeResidualC10(t *testing.T) {
	r := newDelRig(t)
	// (a) another process's descriptor, and a descriptor the guard cannot follow, fail closed.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'bash </dev/null </proc/$$/fd/0; :'")
	worktreeDelPipeInterpreterDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'python3 </dev/null </proc/$$/fd/0'")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash 3</proc/$$/fd/0 </dev/null <&3")
	worktreeDelPipeInterpreterDenied(t, r, "printf 'rm -rf ../repo' | python3 3</proc/$$/fd/0 </dev/null <&3")
	// /proc/self/fd/N keeps the c9 bookkeeping: it names this command's own descriptor.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash 3</proc/self/fd/0 </dev/null <&3")
	// (b) a redirection between the wrapper's options and its executable.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | exec -a x >/dev/null bash")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | exec -a x 2>/dev/null bash")
	r.denied(t, "exec -a x >/dev/null rm -rf ../repo", "rm -r ../repo")
	// (c) a shell as the condition of a compound, and a negated shell.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash -c 'if bash; then :; fi'",
		"printf 'rm -rf ../repo' | bash -c 'elif bash; then :; fi'",
		"printf 'rm -rf ../repo' | bash -c 'while bash; do :; done'",
		"printf 'rm -rf ../repo' | bash -c 'until bash; do :; done'",
		"printf 'rm -rf ../repo' | bash -c '! bash'",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// (d) a redirection written before the command name belongs to that command.
	worktreeDelUnreadableDenied(t, r, "<<< 'import shutil; shutil.rmtree(\"../repo\")' python3", "an interpreter program read from a here-string")
	worktreeDelUnreadableDenied(t, r, "<<< 'rm -rf ../repo' bash", "a shell program read from a here-string")
	worktreeDelUnreadableDenied(t, r, "<<< 'rm -rf ../repo' sh", "a shell program read from a here-string")
	// (e) the & of a descriptor duplication is no command separator.
	worktreeDelUnreadableDenied(t, r, "python3 2>&1 <<'PY'\nimport shutil; shutil.rmtree('../repo')\nPY", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "python3 2>&1 <<EOF\nimport os\nEOF", "an interpreter program read from a here-document")
	// (f) a -word is no argument of an option whose arity the guard does not know.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | node --no-warnings --require fs")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 --no-warnings --require fs")
	// (g) the recursion carries the standard input the guard computed for the intermediate shell, so a shell whose own
	// descriptor 0 is a known file hands no pipe to its program. The previous head denied this row.
	r.allowed(t,
		"printf x | bash -c 'bash -c bash </dev/null'",
		"printf x | bash -c 'bash </dev/null'",
	)
	// (h) php's -f/--file naming a standard-input alias, and python's -m code.
	for _, cmd := range []string{
		"printf x | php -f /dev/stdin",
		"printf x | php --file /dev/stdin",
		"printf x | php --file=/dev/stdin",
		"printf x | php -f /proc/self/fd/0",
		"printf x | php -f /dev/fd/0",
		"printf x | python3 -m code",
	} {
		worktreeDelPipeInterpreterDenied(t, r, cmd)
	}
	// The controls the criterion names keep their answers.
	r.allowed(t,
		"printf x | python3 -c 'print(1)'",
		"printf x | python3 -m json.tool",
		"printf x | php -f script.php",
		"printf x | php -r 'echo 1;'",
		"printf x | node script.js",
		"printf x | bash -c 'cat'",
		"printf x | (cd sub; cat)",
		"printf x | nohup cat",
		"printf x | bash -c 'if true; then :; fi'",
		"printf x | bash -c '! true'",
		"cat <<< 'rm -rf ../repo'",
		"cat 2>&1 <<'PY'\nimport os\nPY",
		"exec -a x true",
		"printf x | exec -a x cat",
		"bash 3</proc/self/fd/0",
		"bash </dev/null",
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualC10b is the pre-merge evaluation's second round on head 918798df1, red first there. Four
// forms inside this issue's promise still reached the piped program: (d1) the & of a descriptor duplication ended the
// pipe region before the command word was read; (d2) a program letter inside a short-option cluster did not name its
// program, so python -Im code and php -nf /dev/stdin were not read; (d3) a ! written after the compound keyword was
// read as the command; (d4) the here-document owner scan cut at a separator inside a quote. Each was reproduced with a
// harmless stand-in for the deletion in the host's zsh.
func TestWorktreeDelPipeResidualC10b(t *testing.T) {
	r := newDelRig(t)
	// (d1) the & of a descriptor duplication is no separator for the pipe region either.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | exec -a x 2>&1 bash")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | exec -a x >/dev/null 2>&1 bash")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | 2>&1 bash")
	r.denied(t, "exec -a x 2>&1 rm -rf ../repo", "rm -r ../repo")
	// (d2) the program letter of a cluster names its program wherever it stands in the cluster.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -Im code")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -Em code")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | php -nf /dev/stdin")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -Imcode")
	// php -rf names the program r with the argument f, so it reads no script from the pipe: the program letter that
	// counts is the one that carries the argument, and -r carries it.
	r.allowed(t, "printf x | php -rf /dev/stdin")
	// (d3) a ! written after the compound keyword is stripped too.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash -c 'if ! bash; then :; fi'",
		"printf 'rm -rf ../repo' | bash -c 'elif ! bash; then :; fi'",
		"printf 'rm -rf ../repo' | bash -c 'while ! bash; do :; done'",
		"printf 'rm -rf ../repo' | bash -c 'until ! bash; do :; done'",
		"printf 'rm -rf ../repo' | bash -c 'if ! bash -c bash; then :; fi'",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// (d4) the here-document owner scan reads the shell's separators, not the raw bytes.
	worktreeDelUnreadableDenied(t, r, "python3 2>'a;b' <<'PY'\nimport os\nPY", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "python3 2>'x|y' <<EOF\nimport os\nEOF", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "python3 3>'a;b' <<EOF\nimport os\nEOF", "an interpreter program read from a here-document")
	// The controls keep their answers.
	r.allowed(t,
		"printf x | python3 -Im json.tool",
		"printf x | php -nf script.php",
		"printf x | bash -c 'if true; then :; fi'",
		"printf x | bash -c '! true'",
		"printf x | exec -a x 2>&1 cat",
		"printf x | python3 -c 'print(1)'",
		"cat 'x;y' <<EOF\nimport os\nEOF",
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualC10c is the pre-merge evaluation's third round, on head 350f75c1f, red first there. Six
// forms inside this issue's promise still reached the piped or here program: (d1) the descriptor walk read only a "<",
// so 0>&3 (an output-style duplication back onto descriptor 0) and <>/dev/stdin (a read-write reopen of the same
// alias) left the pipe recorded as a file; (d2) a condition argument named then or do was mistaken for the clause
// introducer, hiding the shell that inherits the pipe; (d3) the end-of-options marker -- was read as an unknown
// argument-taking long option, so python3 -- /dev/stdin ran the pipe; (d4) a here-document whose owner stands behind a
// condition prefix (if python3 <<EOF) was not recognized; (d5) a here-document written on descriptor 3 and copied back
// onto descriptor 0 with <&3 was ignored; (d6) a legitimate file program behind a boolean long option (node
// --no-warnings script.js) was newly denied. Each form was reproduced with a harmless stand-in for the deletion in the
// host's zsh; no row runs a deletion, and the guard reads text and runs nothing.
func TestWorktreeDelPipeResidualC10c(t *testing.T) {
	r := newDelRig(t)
	// (d1) an output-style duplication of the saved pipe onto descriptor 0, and a read-write reopen of an alias.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash -c 'bash 3<&0 </dev/null 0>&3'",
		"printf 'rm -rf ../repo' | bash -c 'bash 3</dev/fd/0 </dev/null 0>&3'",
		"printf 'rm -rf ../repo' | bash -c 'bash <>/dev/stdin'",
		"printf 'rm -rf ../repo' | bash -c 'bash 0<>/dev/fd/0'",
		"printf 'rm -rf ../repo' | bash 3<&0 </dev/null 0>&3",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 3<&0 </dev/null 0>&3")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 <>/dev/stdin")
	// (d2) a condition argument named then or do is no clause introducer: bash -s reads the piped program with then
	// as $1. A bare script operand named then (bash then) is a file bash cannot read, not the introducer, so it runs
	// nothing and stays allowed.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash -c 'if bash -s then; then :; fi'",
		"printf 'rm -rf ../repo' | bash -c 'while bash -s do; do :; done'",
		"printf 'rm -rf ../repo' | bash -c 'if bash -s do; then :; fi'",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	r.allowed(t,
		"printf 'rm -rf ../repo' | bash -c 'if bash then; then :; fi'",
		"printf 'rm -rf ../repo' | bash -c 'if bash do; then :; fi'",
	)
	// (d3) the end-of-options marker ends the option parse, so the word after it is the script operand.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -- /dev/stdin ignored.py")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -- /dev/stdin")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 -O -- /proc/self/fd/0 ignored.py")
	// (d4) a here-document whose owner stands behind a condition prefix.
	worktreeDelUnreadableDenied(t, r, "if python3 <<'PY'\nimport os\nPY\nthen :; fi", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "while python3 <<EOF\nimport os\nEOF\ndo :; done", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "if ! python3 <<EOF\nimport os\nEOF\nthen :; fi", "an interpreter program read from a here-document")
	// (d5) a here-document written on another descriptor and copied back onto descriptor 0.
	worktreeDelUnreadableDenied(t, r, "python3 3<<'PY' <&3\nimport os\nPY", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "python3 3<<EOF <&3\nimport os\nEOF", "an interpreter program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "python3 3<<EOF 4<&3 <&4\nimport os\nEOF", "an interpreter program read from a here-document")
	// (d6) a boolean long option does not swallow the file program behind it.
	r.allowed(t,
		"printf x | node --no-warnings script.js",
		"printf x | node --no-warnings --trace-warnings script.js",
		"printf x | python3 --version script.py",
		"printf x | perl --version script.pl",
	)
	// The c10(f) deny keeps its answer: a -word is no argument of an unknown-arity option.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | node --no-warnings --require fs")
	// The controls keep their answers.
	r.allowed(t,
		"printf x | python3 -c 'print(1)'",
		"printf x | node script.js",
		"printf x | python3 -m json.tool",
		"cat <>/dev/null <<EOF\nimport os\nEOF",
		"python3 3<<EOF\nignored\nEOF",
		"python3 -c 'print(1)' <<EOF\nx\nEOF",
		"printf x | bash -c 'if true; then :; fi'",
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualC10d is the pre-merge evaluation's fourth round, red first on head 8d7b38fe1. Four forms
// inside this issue's promise still reached the piped program: (d1) a lone - after the end-of-options marker was read
// as a file script operand, so python3 -- - read the pipe; (d2) a redirection written between exec's -a and its name
// argument was consumed as that argument, so exec -a >/dev/null x bash hid the shell; (d3) the descriptor walk read a
// -c or eval program's quoted text as a redirection of the outer command, so eval "0</dev/null; bash" lost the pipe
// the inner shell inherits; (d4) an interpreter's script operand naming a descriptor the command itself opened was
// read against descriptor 0 only, so python3 /dev/fd/3 3<&0 </dev/null read the pipe while the guard saw a file.
// Each was reproduced with a harmless stand-in for the deletion in the host's zsh; no row runs a deletion, and the
// guard reads text and runs nothing.
func TestWorktreeDelPipeResidualC10d(t *testing.T) {
	r := newDelRig(t)
	// (d1) a lone - after -- is the standard-input operand.
	for _, cmd := range []string{
		"printf x | python3 -- -",
		"printf x | python3 -O -- -",
		"printf x | node -- -",
	} {
		worktreeDelPipeInterpreterDenied(t, r, cmd)
	}
	// (d2) a redirection between the wrapper's option and its argument belongs to the command.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | exec -a >/dev/null x bash",
		"printf 'rm -rf ../repo' | exec -a 2>/dev/null x bash",
		"printf 'rm -rf ../repo' | exec -a >/dev/null x sh",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	r.denied(t, "exec -a >/dev/null x rm -rf ../repo", "rm -r ../repo")
	// (d3) a quoted -c or eval program is text, not a redirection of the outer command.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'eval \"0</dev/null; bash\"'")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | bash -c 'eval \"0</dev/null; bash -c bash\"'")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | eval '0</dev/null; bash'")
	// (d4) an interpreter's script operand names a descriptor of this command.
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 /dev/fd/3 3<&0 </dev/null")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 /dev/fd/3 3<&0 0</dev/null")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | python3 /dev/stdin </dev/null")
	worktreeDelPipeInterpreterDenied(t, r, "printf x | php -f /dev/fd/3 3<&0 </dev/null")
	// (d2 continued) a zsh precommand modifier and a wrapper's end-of-options marker name the command behind
	// them: noglob and nocorrect hand their command the shell's standard input, and env -- bash runs bash
	// (CRW-894, the independent review's fourth round).
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | noglob bash",
		"printf 'rm -rf ../repo' | nocorrect bash",
		"printf 'rm -rf ../repo' | env -- bash",
		"printf 'rm -rf ../repo' | nice -- bash",
		"printf 'rm -rf ../repo' | env --ignore-environment bash",
		"printf 'rm -rf ../repo' | sudo -- bash",
		"printf 'rm -rf ../repo' | timeout -- 5 bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	worktreeDelPipeInterpreterDenied(t, r, "printf x | noglob python3")
	// The controls keep their answers.
	r.allowed(t,
		"printf x | noglob cat",
		"printf x | env -- cat",
		"printf x | env -u FOO cat",
		"printf x | sudo -u root cat",
		"printf x | python3 -- script.py",
		"printf x | python3 -- /dev/null",
		"printf x | python3 script.py",
		"printf x | python3 /dev/null",
		"printf x | exec -a x cat",
		"printf x | bash -c 'cat'",
		"printf x | python3 -c 'print(1)'",
		"printf x | python3 /dev/fd/3 3</dev/null",
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualC10e is the pre-merge evaluation fifth round, red first on head 65a24b8bc. Five forms
// inside this issue promise still reached the piped program, and two were over-denials this change introduced:
// (d1) the new option-argument skip stepped over a quoted argument that only looks like a redirection after quote
// removal, so `env -u ">/dev/null" rm -rf ../repo` was allowed and `env -u ">/dev/null" bash` hid the shell;
// (d2) the descriptor walk read a shell argument that only looks like a redirection after quote removal as a real one,
// so `bash -c 'bash -s "</dev/null"'` lost the pipe; (d3) MULTIOS was applied to descriptor 0 only, so
// `python3 /dev/fd/3 3<&0 3</dev/null` read the pipe on descriptor 3; (d4) a condition prefix before a here-string and
// a subshell opener written without a blank before the command name hid the interpreter owner; (d5) an exec builtin
// with only redirections did not carry its descriptor change to the commands after it, so `bash -c 'exec </dev/null;
// bash'` was denied although the inner shell reads the file. Each was reproduced with a harmless stand-in for the
// deletion in the host zsh; no row runs a deletion, and the guard reads text and runs nothing.
func TestWorktreeDelPipeResidualC10e(t *testing.T) {
	r := newDelRig(t)
	// (d1) a quoted argument that only looks like a redirection is the option argument it is.
	r.denied(t, `env -u ">/dev/null" rm -rf ../repo`, "rm -r ../repo")
	r.denied(t, `exec -a ">/dev/null" rm -rf ../repo`, "rm -r ../repo")
	worktreeDelPipeDenied(t, r, `printf 'rm -rf ../repo' | env -u ">/dev/null" bash`)
	r.allowed(t, `env -u ">/dev/null" rm -rf ../other`, `exec -a ">/dev/null" rm -rf ../other`)
	// (d2) a shell argument that only looks like a redirection changes no descriptor.
	worktreeDelPipeDenied(t, r, `printf 'rm -rf ../repo' | bash -c 'bash -s "</dev/null"'`)
	worktreeDelPipeDenied(t, r, `printf 'rm -rf ../repo' | bash -c 'bash "</dev/null"'`)
	// (d3) MULTIOS reaches the descriptor that holds the pipe, not only descriptor 0.
	worktreeDelPipeInterpreterDenied(t, r, `printf x | python3 /dev/fd/3 3<&0 3</dev/null`)
	worktreeDelPipeDenied(t, r, `printf 'rm -rf ../repo' | bash 3<&0 3</dev/null`)
	// (d4) the condition prefix and a subshell opener written without a blank do not hide the owner.
	worktreeDelUnreadableDenied(t, r, `if python3 <<< 'import os'; then :; fi`, "an interpreter program read from a here-string")
	worktreeDelUnreadableDenied(t, r, "(python3 <<'PY'\nimport os\nPY\n)", "an interpreter program read from a here-document")
	// (d5) an exec builtin with only redirections carries its descriptor change to the commands after it.
	r.allowed(t,
		`printf x | bash -c 'exec </dev/null; bash'`,
		`printf x | bash -c 'exec </dev/null; cat'`,
	)
	worktreeDelPipeDenied(t, r, `printf 'rm -rf ../repo' | bash -c 'exec bash'`)
	worktreeDelPipeDenied(t, r, `printf 'rm -rf ../repo' | bash -c 'cat; bash'`)
	// The controls keep their answers.
	r.allowed(t,
		`printf x | env -u FOO cat`,
		`printf x | bash -c 'cat'`,
		`printf x | python3 /dev/fd/3 3</dev/null`,
		`printf x | python3 -c 'print(1)'`,
	)
	r.intact(t)
}

// TestWorktreeDelPipeResidualC10f is this change's own independent review's fifth round, red first on head 65a24b8bc.
// A here-document or here-string written after the word that closes a compound or a group feeds the whole compound,
// and the shell or interpreter inside it that reads standard input reads that body: the owner word is done, fi, esac,
// } or ), not a command, so the reading judged the closer as a command name and allowed the body. Each form was
// reproduced with a harmless stand-in for the deletion in bash 5.3.9 and zsh 5.9; no row runs a deletion, and the
// guard reads text and runs nothing.
func TestWorktreeDelPipeResidualC10f(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"while python3; do :; done <<'PYEOF'\nimport os\nPYEOF",
		"if python3; then :; fi <<'PYEOF'\nimport os\nPYEOF",
		"for i in 1; do python3; done <<'PYEOF'\nimport os\nPYEOF",
		"{ python3; } <<'PYEOF'\nimport os\nPYEOF",
		"(python3) <<'PYEOF'\nimport os\nPYEOF",
	} {
		worktreeDelUnreadableDenied(t, r, cmd, "an interpreter program read from a here-document")
	}
	worktreeDelUnreadableDenied(t, r, "while bash; do :; done <<'SHEOF'\ntouch x\nSHEOF", "a shell program read from a here-document")
	// A compound whose body reads no standard input keeps its answer.
	r.allowed(t,
		"cat <<'EOF'\nimport os\nEOF",
		"while cat; do :; done <<'EOF'\nimport os\nEOF",
		"while python3 -c 'print(1)'; do :; done <<'PYEOF'\nimport os\nPYEOF",
	)
	r.intact(t)
}
