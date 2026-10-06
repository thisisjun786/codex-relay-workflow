package hook

import (
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/state"
)

// CRW-726 generation 2 (c15): the pipe rule of this change, closed against the five findings of the parent's
// independent review of head 71cf063d. Each row was checked in bash 5.3.9 with a harmless stand-in for rm (touch in a
// temporary directory); no row runs a deletion, and the guard reads text and runs nothing.

// worktreeDelPipeDenied asserts a deny of the pipe position.
func worktreeDelPipeDenied(t *testing.T, r delRig, cmd string) {
	t.Helper()
	worktreeDelUnreadableDenied(t, r, cmd, "a shell program read from a pipe")
}

// TestWorktreeDelPipeStdinDescriptor is c15(a): only a word that opens a file on descriptor 0 replaces the pipe. A
// descriptor duplication or a close keeps the refusal, because bash still reads the piped program.
func TestWorktreeDelPipeStdinDescriptor(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | bash <&0",
		"printf 'rm -rf ../repo' | bash 0<&0",
		"printf 'rm -rf ../repo' | bash <&2",
		"printf 'rm -rf ../repo' | bash 0<&2",
		"printf 'rm -rf ../repo' | bash <&-",
		"printf 'rm -rf ../repo' | bash 0<&-",
		"printf 'rm -rf ../repo' | bash 2</dev/null",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A file on descriptor 0 replaces the pipe: bash reads the file, not the pipe.
	r.allowed(t,
		"printf 'rm -rf ../repo' | bash </dev/null",
		"printf 'rm -rf ../repo' | bash < /dev/null",
		"printf 'rm -rf ../repo' | bash 0</dev/null",
		"printf 'rm -rf ../repo' | bash 0< /dev/null",
	)
	r.intact(t)
}

// TestWorktreeDelPipeCompound is c15(b): a subshell or a brace group on the right of a pipe runs every simple command
// in it with the pipe on standard input.
func TestWorktreeDelPipeCompound(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | (true; bash)",
		"printf 'rm -rf ../repo' | (cd sub; bash)",
		"printf 'rm -rf ../repo' | ( exec 2>/dev/null; bash )",
		"printf 'rm -rf ../repo' | (cd sub && bash)",
		"printf 'rm -rf ../repo' | { bash; }",
		"printf 'rm -rf ../repo' | { cd sub; bash; }",
		"printf 'rm -rf ../repo' | (bash)",
		"printf 'rm -rf ../repo' | ( sh )",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// The pipe belongs to the shell only: a compound command that runs something else is read as today.
	r.allowed(t,
		"printf x | (cd sub; cat)",
		"printf x | { cat; }",
		"printf x | (bash -c 'cat')",
	)
	r.intact(t)
}

// TestWorktreeDelPipeWrapper is c15(c): the pipe rule skips the same wrapper set the walk's program reader skips.
func TestWorktreeDelPipeWrapper(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | nohup bash",
		"printf 'rm -rf ../repo' | timeout 5 bash",
		"printf 'rm -rf ../repo' | setsid bash",
		"printf 'rm -rf ../repo' | nice bash",
		"printf 'rm -rf ../repo' | stdbuf -o0 bash",
		"printf 'rm -rf ../repo' | exec bash",
		"printf 'rm -rf ../repo' | time bash",
		"printf 'rm -rf ../repo' | env bash",
		"printf 'rm -rf ../repo' | command bash",
		"printf 'rm -rf ../repo' | sudo bash",
		"printf 'rm -rf ../repo' | xargs bash",
		"printf 'rm -rf ../repo' | ionice bash",
		"printf 'rm -rf ../repo' | builtin bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A wrapper around a command that is no shell reads the pipe as its own input.
	r.allowed(t,
		"printf x | nohup cat",
		"printf x | timeout 5 cat",
		"printf x | nice cat",
	)
	r.intact(t)
}

// TestWorktreeDelRemovalWrapper is c15(d): stripPrefixes skips the same wrapper set, so a removal behind a wrapper is
// named and denied.
func TestWorktreeDelRemovalWrapper(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow, what string }{
		{"nohup rm -rf ../repo", "nohup rm -rf ../other", "rm -r ../repo"},
		{"timeout 5 rm -rf ../repo", "timeout 5 rm -rf ../other", "rm -r ../repo"},
		{"nice rm -rf ../repo", "nice rm -rf ../other", "rm -r ../repo"},
		{"nice -n 5 rm -rf ../repo", "nice -n 5 rm -rf ../other", "rm -r ../repo"},
		{"setsid rm -rf ../repo", "setsid rm -rf ../other", "rm -r ../repo"},
		{"stdbuf -o0 rm -rf ../repo", "stdbuf -o0 rm -rf ../other", "rm -r ../repo"},
		{"ionice rm -rf ../repo", "ionice rm -rf ../other", "rm -r ../repo"},
		{"xargs rm -rf ../repo", "xargs rm -rf ../other", "rm -r ../repo"},
		{"exec rm -rf ../repo", "exec rm -rf ../other", "rm -r ../repo"},
	} {
		r.denied(t, c.deny, c.what)
		r.allowed(t, c.allow)
	}
	r.intact(t)
}

// TestMemoryGatePipeWrapper is c15(e): the memory gate reads the same check, so a wrapped or compound shell behind a
// pipe is a write attempt of its own.
func TestMemoryGatePipeWrapper(t *testing.T) {
	cwd, root, env := gateScene(t)
	for _, command := range []string{
		"printf 'echo x > " + root + "/a' | nohup bash",
		"printf 'echo x > " + root + "/a' | (cd x; bash)",
		"printf 'echo x > " + root + "/a' | bash <&0",
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
}

// TestWorktreeDelPipeReviewFindings pins the eight blockers the generation-2 independent review of head 4b6bc82f5
// raised. Three were fail-opens of this change's own pipe rule, one was a wrapper option argument, one an unbalanced
// delimiter that swallowed the scan, one an over-denial on here-document data, one quadratic work, and one a break of
// the oracle first walk's byte-for-byte parity. Each is fixed and stays pinned.
func TestWorktreeDelPipeReviewFindings(t *testing.T) {
	r := newDelRig(t)
	// 1-3: a compound right side keeps the pipe through a trailing redirection, through nesting and through a shell
	// compound's keywords.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | (bash) 2>/dev/null",
		"printf 'rm -rf ../repo' | (bash) >/dev/null",
		"printf 'rm -rf ../repo' | (bash) 2>&1",
		"printf 'rm -rf ../repo' | { bash; } 2>/dev/null",
		"printf 'rm -rf ../repo' | (true; bash) 2>/dev/null",
		"printf 'rm -rf ../repo' | ( ( bash ) )",
		"printf 'rm -rf ../repo' | { (bash); }",
		"printf 'rm -rf ../repo' | if true; then bash; fi",
		"printf 'rm -rf ../repo' | while true; do bash; done",
		"printf 'rm -rf ../repo' | for i in 1; do bash; done",
		"printf 'rm -rf ../repo' | case x in x) bash;; esac",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// 4: a wrapper option that takes a separate argument does not hide the shell.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | sudo -u root bash",
		"printf 'rm -rf ../repo' | env -u FOO bash",
		"printf 'rm -rf ../repo' | sudo -p prompt bash",
		"printf 'rm -rf ../repo' | timeout -s TERM 5 bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// 5: an unbalanced delimiter does not swallow the rest of the scan.
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | tee { | bash")
	worktreeDelPipeDenied(t, r, "printf 'rm -rf ../repo' | echo { | bash")
	// 6: here-document data is no pipe region.
	r.allowed(t, "cat <<'EOF'\necho hi | bash\nEOF", "cat > run.sh <<'EOF'\ncurl x | bash\nEOF")
	// The controls stay allowed.
	r.allowed(t, "printf 'echo hi' | bash </dev/null", "printf x | (cd sub; cat)", "printf x | { cat; }", "printf x | nohup cat", "printf x | bash -c 'cat'", "nohup rm -rf ../other")
	// 8: the oracle's own first walk is unchanged; the extended walk adds the deny.
	for _, cmd := range []string{
		"nohup rm -rf ../repo", "timeout 5 rm -rf ../repo", "nice rm -rf ../repo", "setsid rm -rf ../repo",
		"stdbuf -o0 rm -rf ../repo", "ionice rm -rf ../repo", "xargs rm -rf ../repo", "exec rm -rf ../repo",
		"time rm -rf ../repo", "X=1 rm -rf ../repo", "-x rm -rf ../repo", "nohup git worktree remove ../repo",
	} {
		if v := worktreeDelWalk(cmd, r.checkout, r.id(), false); v.Deny {
			t.Errorf("first walk parity: %q must be allowed by the oracle's own walk (%s)", cmd, v.Reason)
		}
		if v := r.verdict(cmd); !v.Deny {
			t.Errorf("extended walk: %q must be denied", cmd)
		}
	}
	r.intact(t)
}

// TestWorktreeDelPipeScanIsLinear is the seventh finding: the pipe scan is one forward pass, so a long command line
// is read in linear time and never reaches the hook's timeout.
func TestWorktreeDelPipeScanIsLinear(t *testing.T) {
	r := newDelRig(t)
	for _, size := range []int{32768, 131072, 1048576} {
		command := "printf x | " + strings.Repeat("echo a ", size/7)
		if got, ok := worktreeDelVerdictWithin(t, r, command, 20*time.Second); !ok {
			t.Fatalf("the verdict for %d bytes did not return within 20s", size)
		} else if got.Deny {
			t.Errorf("%d bytes: denied (%s); want allow", size, got.Reason)
		}
	}
	r.intact(t)
}

// TestWorktreeDelPipeCompoundBodies pins the compound bodies the generation-2 re-review's probing reached: a case, a
// while, a for and an if inside a subshell, a while inside a brace group, and a subshell nested in a subshell. Every
// one of them runs the shell with the pipe on its standard input (checked in bash 5.3.9 with a touch stand-in for rm).
// The region a pipe feeds runs to the end of the text when it opens a compound, because a compound's own separators
// and its case patterns make its end hard to find and reading past it can only deny too much.
func TestWorktreeDelPipeCompoundBodies(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | (case x in x) echo esac ; bash;; esac)",
		"printf 'rm -rf ../repo' | (while true; do echo done ; bash; break; done)",
		"printf 'rm -rf ../repo' | (for i in x; do bash; break; done)",
		"printf 'rm -rf ../repo' | (if true; then bash; fi)",
		"printf 'rm -rf ../repo' | ( echo done ; bash )",
		"printf 'rm -rf ../repo' | { while true; do bash; break; done; }",
		"printf 'rm -rf ../repo' | ( ( while true; do bash; break; done ) )",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A compound that runs something other than a shell is read as today, and the controls stay allowed.
	r.allowed(t, "printf x | (cd sub; cat)", "printf x | { cat; }", "printf x | bash -c 'cat'", "printf x | nohup cat")
	r.intact(t)
}

// TestWorktreeDelPipeOperatorForms pins the operator forms a real shell accepts: |& pipes standard error as well as
// standard output, and a pipeline may break across a newline (or a blank line) after its operator, so the command on
// the next line still reads the pipe. Both were allowed on this head while bash ran the piped program (checked in
// bash 5.3.9 with a touch stand-in for rm); a comment after the operator keeps the refusal it already had.
func TestWorktreeDelPipeOperatorForms(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' |& bash",
		"printf 'rm -rf ../repo' |& (true; bash)",
		"printf 'rm -rf ../repo' |& nohup bash",
		"printf 'rm -rf ../repo' |&\nbash",
		"printf 'rm -rf ../repo' |\nbash",
		"printf 'rm -rf ../repo' | \n bash",
		"printf 'rm -rf ../repo' |\n\nbash",
		"printf 'rm -rf ../repo' | # c\nbash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A command that is no shell reads the pipe as its own input, however the operator is written.
	r.allowed(t,
		"printf x |& cat",
		"printf x |& nohup cat",
		"printf x |\ncat",
		"printf x | # c\ncat",
	)
	r.intact(t)
}

// TestWorktreeDelPipeQuoteAndChain pins the other two directions the operator-form fix had to keep right: a | inside a
// quote is data, never an operator, so a quoted pipeline stays allowed, and a chain of pipes still judges the shell at
// its end.
func TestWorktreeDelPipeQuoteAndChain(t *testing.T) {
	r := newDelRig(t)
	// A quoted | is no operator: none of these runs a shell on a pipe.
	r.allowed(t,
		"echo \"a | bash\"",
		"echo 'x | bash'",
		"echo \"a |& bash\"",
		"printf x | grep -e 'a|bash'",
	)
	// A shell at the end of a chain still reads the pipe.
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | cat | bash",
		"printf 'rm -rf ../repo' | grep x |& bash",
		"printf 'rm -rf ../repo' | grep x |\nbash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	r.intact(t)
}

// TestWorktreeDelPipeBraceHash pins the ${B}...} expansion in the pipe scan: a # inside a parameter expansion is data and
// never opens a comment (CRW-726, c13), so the | and the shell after the expansion are still read. The scan's reader
// must carry the expansion depth, or the # of ${x:- #} opens a comment and swallows the rest of the line: on the head
// this test was written against, printf 'rm -rf ../repo' ${x:- #} | bash was allowed while bash ran the piped
// program (checked in bash 5.3.9 with a touch stand-in for rm).
func TestWorktreeDelPipeBraceHash(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' ${x:- #} | bash",
		"printf 'rm -rf ../repo' ${x:-a #b} | bash",
		"printf 'rm -rf ../repo' ${x:- #} |& bash",
		"printf 'rm -rf ../repo' ${x:- #} |\\nbash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A # outside any expansion is still a comment, and a command that is no shell stays allowed.
	r.allowed(t,
		"printf x | # c\\ncat",
		"printf x ${x:- #} | cat",
		"echo ${x:- #}",
	)
	r.intact(t)
}

// TestWorktreeDelPipeCompoundSelect is the shell's select loop: like for, while and until it is a compound whose body
// runs with the pipe on standard input, so a shell in its body reads the piped program (bash 5.3.9 runs it when the
// menu choice is read from the pipe). select was missing from the compound keyword list, so the region a pipe feeds
// ended at the first separator and the body was never judged.
func TestWorktreeDelPipeCompoundSelect(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' | select x in a; do bash; break; done",
		"printf 'rm -rf ../repo' | select x in a\ndo bash\ndone",
		"printf 'rm -rf ../repo' | select x; do bash; done",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A select body that runs something else stays allowed.
	r.allowed(t, "printf x | select y in a; do cat; break; done", "printf x | select y in a; do echo $y; done")
	r.intact(t)
}

// TestWorktreeDelPipeBraceQuotedClose pins the ${...} expansion whose closing } stands inside quotes or is escaped:
// bash reads such a } as data and ends the expansion at the real one, so the # after it is no comment and the | and
// the shell are read. A counter that decrements on any } closes the expansion early, the # then opens a comment and
// swallows the rest of the line: on the head this test was written against, printf 'rm -rf ../repo' ${x:-"}" #} | bash
// was allowed while bash ran the piped program (checked in bash 5.3.9 with a touch stand-in for rm).
func TestWorktreeDelPipeBraceQuotedClose(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf 'rm -rf ../repo' ${x:-\"}\" #} | bash",
		"printf 'rm -rf ../repo' ${x:-a\"}\"b #} | bash",
		"printf 'rm -rf ../repo' ${x:-\"}\" #} |& bash",
		"printf 'rm -rf ../repo' ${x:-\"}\" #} |\\nbash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A } that really ends the expansion, and a # right after the expansion, stay as they were.
	r.allowed(t,
		"echo ${x:- #}",
		"printf x ${x:- #} | cat",
		"printf x | # c\\ncat",
	)
	r.intact(t)
}

// TestWorktreeDelPipeBraceHashAfterPipe pins the ${...} expansion when it stands after a first pipe: every reader of
// the pipe scan must skip the expansion, not only the outer loop. A reader that steps the # of ${x:- #} as a
// comment runs the region to the end of the text, so the second | and the shell after it are never read: on the head
// this test was written against, printf x | printf 'rm -rf ../repo' ${x:- #} | bash was allowed while bash ran the
// printed program (checked in bash 5.3.9 with a touch stand-in for rm).
func TestWorktreeDelPipeBraceHashAfterPipe(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"printf x | printf 'rm -rf ../repo' ${x:- #} | bash",
		"printf x | printf 'rm -rf ../repo' ${x:- #} |& bash",
		"printf x | printf 'rm -rf ../repo' ${x:- #} |\\nbash",
		"printf x | echo 'rm -rf ../repo' ${x:- #b} | bash",
		"printf 'rm -rf ../repo' | cat ${x:- #} | bash",
	} {
		worktreeDelPipeDenied(t, r, cmd)
	}
	// A | inside the expansion is data, and a shell that only reads the expansion's text stays allowed.
	r.allowed(t,
		"printf x ${y:-a | bash } | cat",
		"printf x | printf 'rm -rf ../repo' ${x:- #} | cat",
		"echo ${x:-a | bash}",
	)
	r.intact(t)
}
