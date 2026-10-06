package hook

import (
	"strings"
	"testing"
	"time"
)

// The words after a -c program, the substitutions a word runs before its shell starts, and the reading depth limit, as
// CRW-670 closes the gap CRW-639 left. Every row was run in bash 5.3.9 with a harmless stand-in for rm (touch in a temporary
// directory) and bash -x, so a denied row runs the removal and an allowed row does not; no row runs a deletion. A command or
// process substitution in double quotes or outside quotes is performed by the outer shell before the shell word starts, so its
// program is read like the substitution of any other word, while a substitution in single quotes stays data. The operands after
// the -c program of sh, bash, dash and ash are the inner shell's $0, $1 and so on, option-like ones included, so a later -c is
// not a program for them. A review of this change added rows for four further readings the body reader had missed: a
// substitution in a cd argument, a # after a backtick substitution, a ${...} parameter expansion, and a quote inside the body.
func TestWorktreeDelOperandSubstitutions(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"bash -c 'echo OK' \"$(rm -rf ../repo)\"", // the outer shell runs the substitution before bash starts
		"bash -c 'echo OK' \"`rm -rf ../repo`\"",
		"sh -c 'true' x \"$(rm -rf ../repo)\"", // the substitution is an operand after the program
	} {
		r.denied(t, cmd, "rm -r ../repo")
	}
	r.denied(t, "sh -c 'echo OK' x \"$(rm -rf ../repo)\"", "rm -r ../repo") // the same for sh, whose later operands are data too
	r.denied(t, "bash -c 'echo OK' <(rm -rf ../repo)", "rm -r ../repo")     // a process substitution too
	// The body reader follows the whole substitution, not just up to the first parenthesis it meets: a cd is judged before it
	// moves the directory, a # after a backtick substitution does not open a comment, a ${...} parameter expansion does not
	// close the substitution, and a quote inside the body does not end the outer double quote.
	r.denied(t, "cd \"$(rm -rf ../repo; pwd)\"", "rm -r ../repo")
	r.denied(t, "echo `true`#`rm -rf ../repo`", "rm -r ../repo")
	r.denied(t, "echo \"`# '`\"; rm -rf ../repo", "rm -r ../repo") // a # in a backtick body still ends at the closing backtick
	r.denied(t, "echo \"`'`\"; rm -rf ../repo", "rm -r ../repo")   // an unterminated quote in a backtick body still ends at the closing backtick
	r.denied(t, ": \"$($()#)\"; r\\m -r ../repo", "rm -r ../repo") // a substitution the body reader cannot close falls back to the plain reading
	r.denied(t, "echo \"$(echo ${x:-)}; rm -rf ../repo)\"", "rm -r ../repo")
	r.denied(t, "echo \"$(echo ${x}#)\"; rm -rf ../repo", "rm -r ../repo") // a # right after a ${...} is part of the word, not a comment
	r.denied(t, "bash -c 'echo OK' \"$(echo \")\"; rm -rf ../repo)\"", "rm -r ../repo")
	r.denied(t, "echo \"$(case x in x) : ;; esac; rm -rf ../repo)\"", "rm -r ../repo") // the rest of the segment is judged with the body
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
		"echo \"$(rm -rf ../other)\"", // a substitution in a plain word resolves outside the worktree
	)
	r.denied(t, "bash -c 'eval \"$0\"' 'rm -rf ../repo'", "rm -r ../repo")
	r.denied(t, "bash -c 'eval `printf \"\\x24\\x31\"`' -c 'rm -rf ../repo'", "rm -r ../repo")                                         // a program that synthesizes $1 is not certain
	r.denied(t, "bash -c 'declare -p | grep \"^declare -a BASH_AR.V=\" | cut -d\\\" -f2 | bash' -c 'rm -rf ../repo'", "rm -r ../repo") // an obfuscated BASH_ARGV and a pipeline make the program uncertain
	r.denied(t, "bash -c 'echo $1' -c 'rm -rf ../repo'", "rm -r ../repo")
	r.denied(t, "su -c 'echo ok' -c 'rm -rf ../repo' root", "rm -r ../repo")
	r.intact(t)
}

// A long but harmless command line with many substitutions must still be judged. The walk used to judge the rest of
// a segment after every opener as a nested program, so k backtick substitutions cost about 2^k walks: 2.6 s at k=12
// and far past the PreToolUse hook's ten-second timeout at k=40. The walk now judges that rest at the segment's own
// depth and memoizes, so the same commands answer quickly. Each verdict runs in its own goroutine, so a regression
// fails this test with the timeout instead of hanging the suite (CRW-670, generation 2).
func TestWorktreeDelOperandManySubstitutions(t *testing.T) {
	r := newDelRig(t)
	const many = 40
	for _, c := range []struct{ label, cmd, what string }{
		{"unquoted backticks", "echo" + strings.Repeat(" `true`", many), ""},
		{"quoted backticks", "echo \"" + strings.Repeat("`true` ", many) + "\"", ""},
		{"unquoted backticks then rm", "echo" + strings.Repeat(" `true`", many) + "; rm -rf ../repo", "rm -r ../repo"},
		{"quoted backticks then rm", "echo \"" + strings.Repeat("`true` ", many) + "\"; rm -rf ../repo", "rm -r ../repo"},
	} {
		got, ok := worktreeDelVerdictWithin(t, r, c.cmd, 10*time.Second)
		if !ok {
			t.Fatalf("%s: the verdict did not return within 10s", c.label)
		}
		if c.what == "" {
			if got.Deny {
				t.Errorf("%s: denied (%s); want allow", c.label, got.Reason)
			}
			continue
		}
		if !got.Deny || !strings.HasPrefix(got.Reason, "[crw: WORKTREE-GUARD-03] blocked `"+c.what+"`: ") {
			t.Errorf("%s: deny %v, reason %.60q; want a deny of %q, not the budget reason", c.label, got.Deny, got.Reason, c.what)
		}
	}
	r.intact(t)
}

// worktreeDelVerdictWithin runs one verdict in its own goroutine and reports whether it returned within d.
func worktreeDelVerdictWithin(t *testing.T, r delRig, cmd string, d time.Duration) (GuardVerdict, bool) {
	t.Helper()
	done := make(chan GuardVerdict, 1)
	go func() { done <- r.verdict(cmd) }()
	select {
	case v := <-done:
		return v, true
	case <-time.After(d):
		return GuardVerdict{}, false
	}
}

// One evaluation may judge only worktreeDelWalkBudget distinct texts. With the budget lowered to 16 the walk denies
// a command whose distinct texts exceed it, with a reason of its own; with the default budget the same harmless command
// is allowed (CRW-670, generation 2).
func TestWorktreeDelOperandBudget(t *testing.T) {
	r := newDelRig(t)
	cmd := "echo" + strings.Repeat(" $(true)", 20)
	if got := worktreeDelEvaluate(cmd, r.checkout, r.id(), 16); !got.Deny ||
		!strings.HasPrefix(got.Reason, "[crw: WORKTREE-GUARD-03] blocked `a command too complex for the guard to read`: ") {
		t.Errorf("budget 16: deny %v, reason %.70q; want a deny with the budget reason", got.Deny, got.Reason)
	}
	if got := r.verdict(cmd); got.Deny {
		t.Errorf("default budget: denied (%s); want allow", got.Reason)
	}
	r.intact(t)
}
