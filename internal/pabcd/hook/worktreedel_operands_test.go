package hook

import "testing"

// The words after a -c program, the substitutions a word runs before its shell starts, and the reading depth limit, as
// CRW-670 closes the gap CRW-639 left. Every row was run in bash 5.3.9 with a harmless stand-in for rm (touch in a temporary
// directory) and bash -x, so a denied row runs the removal and an allowed row does not; no row runs a deletion. A command or
// process substitution in double quotes or outside quotes is performed by the outer shell before the shell word starts, so its
// program is read like the substitution of any other word, while a substitution in single quotes stays data. The operands after
// the -c program of sh, bash, dash and ash are the inner shell's $0, $1 and so on, option-like ones included, so a later -c is
// not a program for them.
func TestWorktreeDelOperandSubstitutions(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"bash -c 'echo OK' \"$(rm -rf ../repo)\"", // the outer shell runs the substitution before bash starts
		"bash -c 'echo OK' \"`rm -rf ../repo`\"",
		"sh -c 'true' x \"$(rm -rf ../repo)\"", // the substitution is an operand after the program
	} {
		r.denied(t, cmd, "rm -r ../repo")
	}
	r.denied(t, "bash -c 'echo OK' <(rm -rf ../repo)", "rm -r ../repo") // a process substitution too
	worktreeDelWrapperNestDenied(t, r, "sh -c x9 true", worktreeDelQuoteNest("true", 9), worktreeDelWrapperDeep)
	r.intact(t)
}

// The operands after the -c program of sh, bash, dash and ash are data, option-like ones included: a later -c is the inner
// shell's $0 and not a program, so bash runs echo only (this row moved here from TestWorktreeDelWrapperUncertainStaysRead). A
// substitution in single quotes is data too, and the existing readings that still deny are unchanged.
func TestWorktreeDelOperandDataStaysData(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"bash -c 'echo OK' -c 'rm -rf ../repo'", // the words after the program are $0 and $1
		"bash -c 'echo OK' '$(rm -rf ../repo)'", // single quotes keep the substitution as data
		"bash -c 'echo OK' \"$(rm -rf ../other)\"",
		"bash -c 'echo OK' \"$(rm -rf ./build)\"",
	)
	r.denied(t, "bash -c 'eval \"$0\"' 'rm -rf ../repo'", "rm -r ../repo")
	r.denied(t, "su -c 'echo ok' -c 'rm -rf ../repo' root", "rm -r ../repo")
	r.intact(t)
}
