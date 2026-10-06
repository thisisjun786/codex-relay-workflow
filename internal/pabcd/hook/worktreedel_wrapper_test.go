package hook

import (
	"strings"
	"testing"
)

// The rows of this file read the program a shell is handed, as CRW-611 left the reader. The sh and bash rows were run in bash
// 5.3.9 (and the zsh one in zsh 5.9) with a stand-in function for rm that prints its arguments: a denied row runs rm on the
// worktree itself (../repo, from the checkout) and an allowed row does not. su cannot run without root and PAM, so its rows
// follow util-linux 2.41.3's option table, applied with getopt(1): -c takes the rest of its cluster (-lcPROGRAM) or the next
// word, options may stand after the user, and the last -c wins.
const worktreeDelWrapperDeep = "a shell program nested past the reading depth"

// worktreeDelWrapperEvalNest hands the program to eval in double quotes, depth times.
func worktreeDelWrapperEvalNest(program string, depth int) string {
	for range depth {
		program = "eval \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(program) + "\""
	}
	return program
}

// worktreeDelWrapperNestDenied asserts a deny of what for a nest, which a message would print at a length of several kilobytes: the
// row is named by its label instead.
func worktreeDelWrapperNestDenied(t *testing.T, r delRig, label, cmd, what string) {
	t.Helper()
	if got := r.verdict(cmd); !got.Deny || !strings.HasPrefix(got.Reason, "[crw: WORKTREE-GUARD-03] blocked `"+what+"`: ") {
		t.Errorf("%s: deny %v, reason %.60q; want a deny of %q", label, got.Deny, got.Reason, what)
	}
}

// A -c program that su takes attached to its cluster, or after other options, is read as the program it is, and so is the
// first operand after -c of a shell whose later operands are data.
func TestWorktreeDelWrapperProgramsDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"su -c'rm -rf ../repo'", // an attached program: its first word read as -crm, so the deletion was allowed
		"su -lc'rm -rf ../repo'",
		"su - root -c'rm -rf ../repo'",
		"su -c 'rm -rf ../repo'",
		"su root -c 'rm -rf ../repo'",             // su permutes: an option after the user is still an option
		"su -c 'echo a' -c 'rm -rf ../repo' root", // the last -c wins, and both are read
		"su --command='rm -rf ../repo' root",
		"su -lc'sudo rm -rf ../repo'",
		"bash -lc 'rm -rf ../repo'",
		"bash -cx 'rm -rf ../repo'", // a cluster of flags: c does not take the rest of it
		"sh -ec 'rm -rf ../repo'",
		"zsh -fc 'rm -rf ../repo'",
		"bash -c 'eval \"$0\"' 'rm -rf ../repo'", // a data operand that the program runs is code
		"su -c 'eval \"$1\"' root 'rm -rf ../repo'",
		"bash -c 'source /dev/stdin' <<< 'rm -rf ../repo'", // so is a word that a redirection feeds to it
		"su -c 'source /dev/stdin' root <<< 'rm -rf ../repo'",
		"bash -c 'declare -p BASH_ARGV | cut -d\" -f2 | bash' x 'rm -rf ../repo'", // a program that reads its operands by name, without a dollar sign
		"su -c 'echo ok' -w '>' -c 'rm -rf ../repo' root",                         // a quoted > reaches the walk like a redirection: su takes it as the argument of -w
		"su -c 'echo ok' --whitelist-environment -w -c 'rm -rf ../repo' root",     // an option whose argument looks like one: the last -c runs rm
		"bash -c 2>/dev/null 'rm -rf ../repo'",                                    // redirections stand before the program: the tokenizer leaves a descriptor as a word of its own
		"bash -c 2> /dev/null 'rm -rf ../repo'",
		"bash -c &>/dev/null 'rm -rf ../repo'",
		"bash -c {fd}>/dev/null 'rm -rf ../repo'",
		"bash -o posix -c 'rm -rf ../repo'",
		"bash -oc posix 'rm -rf ../repo'",
		"bash --rcfile /dev/null -c 'rm -rf ../repo'",
		"su -s /bin/sh -c 'rm -rf ../repo' root",
		"su --se 'rm -rf ../repo' root",
	} {
		r.denied(t, cmd, "rm -r ../repo")
	}
	r.denied(t, worktreeDelSpell("su -c'r<BS><NL>m -rf ../../zk3q'"), "rm -r ../../zk3q") // the continuation reading of an attached program
	r.intact(t)
}

// Only a getopt that gives the rest of a cluster to -c reads an attached program: su (and fish). bash, dash and zsh read -crm as flags,
// fail on the blank and run nothing, and su gives the rest of a cluster to its first option that takes an argument, so -sc is a shell
// named c. The reader does not take these for programs.
func TestWorktreeDelWrapperAttachedNeedsGetopt(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, "bash -c'rm -rf ../repo'", "sh -c'rm -rf ../repo'", "zsh -c'rm -rf ../repo'", "su -sc'rm -rf ../repo' root", "su -gc'rm -rf ../repo' root")
	r.denied(t, "su -lc'rm -rf ../repo' root", "rm -r ../repo")
	r.intact(t)
}

// Only the first operand after -c is the program: the words after it are $0, $1 and so on, which the shell does not read.
func TestWorktreeDelWrapperDataOperandsAllowed(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"bash -c 'echo OK' 'rm -rf ../repo'",
		"sh -c 'echo OK' x 'rm -rf ../repo'",
		"zsh -c 'echo OK' 'rm -rf ../repo'",
		"su -c 'echo ok'",
		"su -c 'echo ok' root 'rm -rf ../repo'",
		"su root -c 'echo ok' 'rm -rf ../repo'",
		"su -lc'echo ok' root 'rm -rf ../repo'",
		"su -s /bin/sh -c 'echo ok' root 'rm -rf ../repo'",
	)
	r.intact(t)
}

// Where the options do not name the -c program for sure, or a word after it could matter, every operand that holds a blank is
// still read, as CRW-611 left it: a shell outside the modeled family, a program the walk cannot place (a lone -, +c, an option
// argument it over-reads, a script instead of -c), a redirection word, an option-like word after the program of su and of every
// shell whose later operands are not data (su would run a later -c, and the walk does not tell those shells apart). Over-reading
// only denies too much.
func TestWorktreeDelWrapperUncertainStaysRead(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"ksh -c 'echo OK' 'rm -rf ../repo'",
		"csh -c 'echo OK' 'rm -rf ../repo'",
		"fish -c 'echo OK' 'rm -rf ../repo'",
		"bash -c - 'rm -rf ../repo'",
		"bash +c 'rm -rf ../repo'",
		"bash script.sh 'rm -rf ../repo'",
		"zsh -c -oshwordsplit 'echo OK' 'rm -rf ../repo'",
		"bash -c 'echo OK' <<< 'rm -rf ../repo'",
		"su --shell /bin/sh 'rm -rf ../repo'",
		"bash -c 'echo OK' > 'log file' 'rm -rf ../repo'",
		"su -c 'echo ok' -- root 'rm -rf ../repo'",
	} {
		r.denied(t, cmd, "rm -r ../repo")
	}
	r.intact(t)
}

// A program string nested past the reading depth is not read, so the walk cannot tell what it runs and denies it.
func TestWorktreeDelWrapperPastTheDepth(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ label, cmd string }{
		{"eval x9 rm", worktreeDelWrapperEvalNest("rm -rf ../repo", 9)},
		{"eval x12 rm", worktreeDelWrapperEvalNest("rm -rf ../repo", 12)},
		{"sh -c x9 rm", worktreeDelQuoteNest("rm -rf ../repo", 9)},
		{"sh -c x12 rm", worktreeDelQuoteNest("rm -rf ../repo", 12)},
		{"eval x9 echo", worktreeDelWrapperEvalNest("echo ok", 9)}, // it is the nest that is denied, not what the innermost text does
	} {
		worktreeDelWrapperNestDenied(t, r, c.label, c.cmd, worktreeDelWrapperDeep)
	}
	// Eight levels are still read down to the deletion itself, and an innermost command that is no program passes.
	worktreeDelWrapperNestDenied(t, r, "eval x8 rm", worktreeDelWrapperEvalNest("rm -rf ../repo", 8), "rm -r ../repo")
	worktreeDelWrapperNestDenied(t, r, "sh -c x8 rm", worktreeDelQuoteNest("rm -rf ../repo", 8), "rm -r ../repo")
	r.allowed(t, worktreeDelWrapperEvalNest("echo ok", 8), worktreeDelQuoteNest("echo ok", 8))
	r.intact(t)
}
