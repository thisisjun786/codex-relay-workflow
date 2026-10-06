package hook

import (
	"slices"
	"strings"
	"testing"
)

// CRW-726: the deletion guard refuses a command whose program position, or whose command name, the outer shell builds
// at run time, because it cannot read what will run before it runs (fail closed). The positions are the -c program of a
// listed shell (sh, bash, dash, ash, zsh, ksh, su, with --command= and su -cPROGRAM), the operands of eval, the first
// operand of source and ., the standard input of a listed shell that stands to the right of a single pipe or takes a
// here-string, and the here-document a listed shell reads as its program.
//
// CXC v0.2.40 allows every one of these forms, so this is a security fix (port: fixed). The shell rows were checked
// against bash 5.3.9 with a harmless stand-in for rm (touch in a temporary directory); no row runs a deletion, and the
// guard reads text and runs nothing.

// worktreeDelUnreadableDenied asserts a deny whose reason names the position, in the shape the issue fixes.
func worktreeDelUnreadableDenied(t *testing.T, r delRig, cmd, what string) {
	t.Helper()
	want := "[crw: WORKTREE-GUARD-03] blocked `a program the guard cannot read before it runs: " + what + "`: "
	if got := r.verdict(cmd); !got.Deny || !strings.HasPrefix(got.Reason, want) {
		t.Errorf("%q: got %+v, want a deny of %q", cmd, got, want)
	}
}

// TestWorktreeDelUnreadableProgramsDenied is c1 and c2: every program position whose word carries an expansion the
// outer shell performs first is denied, and the reason names the position.
func TestWorktreeDelUnreadableProgramsDenied(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ command, what string }{
		{"bash -c \"$(printf 'rm -rf ../repo')\"", "a bash -c program"},
		{"bash -c \"$x\"", "a bash -c program"},
		{"sh -c \"`cat f`\"", "a sh -c program"},
		{"sudo bash -c \"$X\"", "a bash -c program"},
		{"su -c \"$X\" root", "a su -c program"},
		{"su -c'$X' root", "a su -c program"},
		{"su --command='$X' root", "a su -c program"},
		{"eval \"$(printf 'rm -rf ../repo')\"", "an eval operand"},
		{"eval $X", "an eval operand"},
		{"source <(printf 'rm -rf ../repo')", "a source operand"},
		{". <(cat f)", "a source operand"},
		{"source \"$F\"", "a source operand"},
		{"printf 'rm -rf ../repo' | bash", "a shell program read from a pipe"},
		{"cat f | sh", "a shell program read from a pipe"},
		{"bash <<< \"$(printf 'rm -rf ../repo')\"", "a shell program read from a here-string"},
		{"bash -c 'bash -c \"$(printf x)\"'", "a bash -c program"},
	} {
		worktreeDelUnreadableDenied(t, r, c.command, c.what)
	}
	r.intact(t)
}

// TestWorktreeDelUnreadableProgramsAllowed is c2's allowed list: a program the guard can read, a script file, an
// expansion outside a program position and a pipe that feeds no shell stay allowed.
func TestWorktreeDelUnreadableProgramsAllowed(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"bash -c 'echo $1' x y",
		"bash -c \"echo \\$HOME\"",
		"bash -c 'echo OK'",
		"eval echo ok",
		"source ./env.sh",
		"bash script.sh",
		"echo \"$HOME\"",
		"ls | grep x",
		"printf x | bash -c 'cat'",
	)
	r.intact(t)
}

// TestWorktreeDelUnreadableHeredocDenied is c1(e) and c2's added rows: a listed shell that reads its program from a
// here-document with an unquoted delimiter lets the outer shell expand the body first, so a body holding an expansion is
// a program the guard cannot read. A quote in the body protects nothing there, and a body whose closing delimiter line
// is missing never ends.
func TestWorktreeDelUnreadableHeredocDenied(t *testing.T) {
	r := newDelRig(t)
	for _, command := range []string{
		"bash <<EOF\n$(printf 'rm -rf ../repo')\nEOF",
		"bash <<EOF\n`printf 'rm -rf ../repo'`\nEOF",
		"bash <<EOF\n$X\nEOF",
		"bash <<EOF\necho '$X'\nEOF",
		"sh <<-EOF\n\t$(cat f)\n\tEOF",
		"bash -s <<EOF\n$X\nEOF",
		"bash <<EOF\n$X",
		"0<<EOF bash\n$X\nEOF",
	} {
		worktreeDelUnreadableDenied(t, r, command, "a shell program read from a here-document")
	}
	r.intact(t)
}

// TestWorktreeDelUnreadableHeredocAllowed is c2's allowed here-document list: a quoted delimiter keeps the body literal,
// a body with no expansion is readable, an escaped dollar is literal, a script operand makes the body data, and a
// here-document on a command that is no listed shell is no program position.
func TestWorktreeDelUnreadableHeredocAllowed(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"bash <<'EOF'\nprintf hi\nEOF",
		"bash <<'EOF'\necho \"$HOME\"\nEOF",
		"bash <<EOF\nprintf hi\nEOF",
		"bash <<EOF\necho \\$HOME\nEOF",
		"bash script.sh <<EOF\n$X\nEOF",
		"cat <<EOF\n$HOME\nEOF",
	)
	r.intact(t)
}

// TestWorktreeDelUnreadableRawWordsAlign pins the reading the positions come from: the plain words of a segment's raw
// words are exactly the words the walk's own tokenizer makes of the same segment, so a program position is the word the
// walk read.
func TestWorktreeDelUnreadableRawWordsAlign(t *testing.T) {
	for _, segment := range []string{
		"bash -c 'echo OK' x y",
		"bash -c \"$(printf x)\"",
		"rm -rf ..\\/repo",
		"echo $'a\\tb' \"c d\"",
		"bash <<< 'x'",
		"a 2>&1 b",
		"eval -- '+echo' '$X'",
	} {
		got := worktreeDelUnreadablePlainTexts(worktreeDelUnreadableWords(segment))
		want := worktreeDelQuoteTokenize(segment)
		if !slices.Equal(got, want) {
			t.Errorf("worktreeDelUnreadableWords(%q) plain = %q, want %q", segment, got, want)
		}
	}
}

// TestWorktreeDelUnreadableProgramHelper is c1's target-free helper: it names the position and reports no position for a
// command the guard can read. The memory gate (CRW-727) reads the same answer.
func TestWorktreeDelUnreadableProgramHelper(t *testing.T) {
	for _, c := range []struct{ command, what string }{
		{"bash -c \"$(printf x)\"", "a bash -c program"},
		{"eval $X", "an eval operand"},
		{"source <(cat f)", "a source operand"},
		{"cat f | sh", "a shell program read from a pipe"},
		{"bash <<< \"$X\"", "a shell program read from a here-string"},
		{"bash <<EOF\n$X\nEOF", "a shell program read from a here-document"},
	} {
		got, ok := worktreeDelUnreadableProgram(c.command)
		if !ok || got != c.what {
			t.Errorf("worktreeDelUnreadableProgram(%q) = %q, %v; want %q, true", c.command, got, ok, c.what)
		}
	}
	for _, command := range []string{
		"bash -c 'echo OK'",
		"bash -c \"echo \\$HOME\"",
		"echo \"$HOME\"",
		"ls | grep x",
		"bash script.sh",
		"cat <<EOF\n$HOME\nEOF",
	} {
		if got, ok := worktreeDelUnreadableProgram(command); ok {
			t.Errorf("worktreeDelUnreadableProgram(%q) = %q, true; want no position", command, got)
		}
	}
}

// TestWorktreeDelCommentInsideParameterExpansion is c13: bash opens a comment only at the start of a word in plain text,
// so a # inside a ${...} parameter expansion is data, and the backtick or $(...) program that follows such an expansion
// is read and judged (the bypass predates CRW-670, which left this case unfinished).
func TestWorktreeDelCommentInsideParameterExpansion(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, "echo \"$(echo ${x:- #}; echo `rm -rf ../repo`)\"", "rm -r ../repo")
	r.allowed(t,
		"echo \"$(echo ${x:- #}; echo `rm -rf ../other`)\"",
		"echo \"${x:- #}\"",
		"echo ${x:- #} ok",
	)
	r.intact(t)
}

// TestWorktreeDelSubstitutionTailQuoteState is c14(a): the rest of a segment after a substitution is read in the quote
// state its opener stood in, so text in single quotes after the substitution stays data while a substitution the shell
// really performs is still judged.
func TestWorktreeDelSubstitutionTailQuoteState(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, "echo \"$(true)\" '$(rm -rf ../repo)'")
	r.denied(t, "echo \"$(true)\" \"$(rm -rf ../repo)\"", "rm -r ../repo")
	r.intact(t)
}

// TestWorktreeDelSubstitutionCwd is c14(b): a substitution's body is judged from the directory its own segment reached,
// so a cd earlier in the same command moves what a relative target resolves to.
func TestWorktreeDelSubstitutionCwd(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, "cd /tmp; echo \"$(rm -rf ../repo)\"") // bash removes /repo, outside the slot
	r.denied(t, "cd ..; echo \"$(rm -rf repo)\"", "rm -r repo")
	r.intact(t)
}

// TestWorktreeDelCertainProgram is c14(c): whether a -c program can reach the operands the shell hands it decides if the
// walk reads them. A separator, a pipe, a parenthesis or the letters arg inside a word (large, cargo) do not make a
// program uncertain, while a dollar sign, a backtick, <( or >( and a word that names the operands do.
func TestWorktreeDelCertainProgram(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"bash -c 'echo large' 'rm -rf ../repo'",
		"bash -c 'echo OK; true' 'rm -rf ../repo'",
		"bash -c 'echo cargo' 'rm -rf ../repo'",
	)
	r.denied(t, "bash -c 'eval \"$1\"' _ 'rm -rf ../repo'", "rm -r ../repo")
	r.denied(t, "bash -c '\"$@\"' _ rm -rf ../repo", "rm -r ../repo")
	r.denied(t, "bash -c 'echo $1' -c 'rm -rf ../repo'", "rm -r ../repo")
	r.intact(t)
}

// TestWorktreeDelDepthLimitPosition is c14(d): at the reading-depth limit only a segment that hands a program to a shell
// is refused, never an option or a word that is no program position.
func TestWorktreeDelDepthLimitPosition(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, worktreeDelQuoteNest("bash --version", 8), worktreeDelQuoteNest("echo ok", 8))
	worktreeDelWrapperNestDenied(t, r, "sh -c x8 rm", worktreeDelQuoteNest("rm -rf ../repo", 8), "rm -r ../repo")
	worktreeDelWrapperNestDenied(t, r, "sh -c x8 true", worktreeDelQuoteNest("bash -c true", 8), worktreeDelWrapperDeep)
	r.intact(t)
}

// TestWorktreeDelReviewFindings pins the five findings the PR's own Devin review raised. The first two were security
// regressions this change introduced (a nested substitution hidden by the ${...} skip, and quoted text read as a cd)
// and the third an over-denial it introduced (here-document data read as a program); the fourth and fifth were
// over-denial and a bypass in the new reading. Each is fixed here and stays pinned.
func TestWorktreeDelReviewFindings(t *testing.T) {
	r := newDelRig(t)
	// A command substitution nested in a ${...} parameter expansion is still read, in double quotes too.
	r.denied(t, "echo \"${x:-$(rm -rf ../repo)}\"", "rm -r ../repo")
	r.allowed(t, "echo \"${x:-$(rm -rf ../other)}\"", "echo \"${x:-$(true)}\"")
	// Text inside double quotes is no cd of this shell: the substitution after it is judged from the checkout.
	r.denied(t, "echo \"cd /tmp $(rm -rf ../repo)\"", "rm -r ../repo")
	r.allowed(t, "echo \"cd /tmp $(rm -rf ../other)\"")
	// A here-document that feeds a command which is no listed shell is data: no program position is read in it.
	r.allowed(t, "cat <<EOF\nbash -c \"$X\"\nEOF", "cat <<'EOF'\nbash -c \"$X\"\nEOF")
	worktreeDelUnreadableDenied(t, r, "bash <<EOF\n$(printf 'rm -rf ../repo')\nEOF", "a shell program read from a here-document")
	// An option argument is no program position: bash runs the literal script file.
	r.allowed(t, "bash --rcfile \"$X\" script.sh", "bash --init-file \"$X\" script.sh")
	// A leading assignment is no part of the command word, so the name word after it is judged.
	worktreeDelNamedDenied(t, r, "Y=1 $X -rf ../repo", "a command named by an expansion")
	r.allowed(t, "Y=1 \"$GO\" test ./...")
	r.intact(t)
}

// TestWorktreeDelReviewFindingsSecondRound pins the ten findings of the PR's Codex review. Four were bypasses the new
// reading still let through (an eval operand confused with a redirection target, a subshell that kept the pipe, a
// descriptor before a here-document operator, and a program that runs $0), two were over-denials it introduced (a
// here-document that belongs to another command, and a shell whose standard input a redirection replaces), and the rest
// are the two the first round had already fixed.
func TestWorktreeDelReviewFindingsSecondRound(t *testing.T) {
	r := newDelRig(t)
	// A subshell keeps the pipe that fed it, and a here-document with a descriptor is still its shell's program.
	worktreeDelUnreadableDenied(t, r, "printf 'rm -rf ../repo' | (bash)", "a shell program read from a pipe")
	worktreeDelUnreadableDenied(t, r, "bash 0<<EOF\n$(printf 'rm -rf ../repo')\nEOF", "a shell program read from a here-document")
	worktreeDelUnreadableDenied(t, r, "bash 0<<<\"$(printf 'rm -rf ../repo')\"", "a shell program read from a here-string")
	// eval's operands are mapped structurally, so a redirection target that reads like a later operand does not hide it.
	worktreeDelUnreadableDenied(t, r, "eval > '$X' \"$X\"", "an eval operand")
	// A program that runs $0 as well as $@ is read as the command line it builds.
	r.denied(t, "bash -c '\"$0\" \"$@\"' rm -rf ../repo", "rm -r ../repo")
	// A here-document belongs to the command that holds its operator, and a redirection replaces a pipe on stdin.
	r.allowed(t, "cat <<EOF; bash </dev/null\n$X\nEOF", "printf 'rm -rf ../repo' | bash </dev/null")
	worktreeDelUnreadableDenied(t, r, "printf 'rm -rf ../repo' | bash", "a shell program read from a pipe")
	// A script operand and an option argument stay no program position.
	r.allowed(t, "bash script.sh \"$ARG\"", "bash \"$SCRIPT\"", "bash --rcfile \"$X\" script.sh")
	// The first round's five findings stay fixed.
	r.denied(t, "echo \"${x:-$(rm -rf ../repo)}\"", "rm -r ../repo")
	r.denied(t, "echo \"cd /tmp $(rm -rf ../repo)\"", "rm -r ../repo")
	worktreeDelNamedDenied(t, r, "Y=1 $X -rf ../repo", "a command named by an expansion")
	r.allowed(t, "cat <<EOF\nbash -c \"$X\"\nEOF")
	r.intact(t)
}
