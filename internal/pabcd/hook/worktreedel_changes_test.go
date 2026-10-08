package hook

import "testing"

// worktreeDelChange is a verdict the shared command reader (internal/pabcd/shellir) gives on purpose, where the
// old expectation of the test row no longer holds. deny is the verdict now. The reasons are recorded in
// docs/port-cxc/known-defects/CRW-1028.md. TestWorktreeDelChangedVerdicts checks every recorded verdict.
type worktreeDelChange struct {
	cmd  string
	deny bool
}

// worktreeDelChangeRows lists the recorded verdicts.
func worktreeDelChangeRows() []worktreeDelChange {
	return []worktreeDelChange{
		{"-x rm -rf ../repo", false},
		{"xargs rm -rf ../other", true},
		{"echo `echo safe; rm -rf '../repo\\\n'; echo done`", false},
		{"echo `x`; rm -rf '.\\\n'", false},
		{"rm -rf $'.\\\n'", true},
		{"echo a # c \\\nrm -rf ./build", true},
		{"(echo a)#c \\\nrm -rf ./build", true},
		{"echo <<'EOF'\n$'x\\'\nEOF\nrm -rf ../other\\\n; # '", true},
		{"echo a # ` \\\nrm -rf ../other", true},
		{"cat <\\\n<EOF\n$'x\\'\nEOF\nrm -rf ../other\\\n", true},
		{"\"$GO\" test ./...", true},
		{"$EDITOR notes.md", true},
		{"\"$RELAY\" --state /tmp/x show", true},
		{"bash -c 'echo $1' -c 'rm -rf ../repo'", false},
		{"printf 'rm -rf ../repo' | bash </dev/null", true},
		{"printf 'rm -rf ../repo' | bash < /dev/null", true},
		{"printf 'rm -rf ../repo' | bash 0</dev/null", true},
		{"printf 'rm -rf ../repo' | bash 0< /dev/null", true},
		{"printf 'echo hi' | bash </dev/null", true},
		{"printf 'rm -rf ../repo' ${x:- #} |\\nbash", false},
		{"printf x | # c\\ncat", true},
		{"printf 'rm -rf ../repo' ${x:-\"}\" #} |\\nbash", false},
		{"printf x | printf 'rm -rf ../repo' ${x:- #} |\\nbash", false},
		{"cd \\.\\.; rm -rf other", true},
		{"rm -rf $'\\x2e\\x2e/other'", true},
		{"rm -rf $'\\056\\056/other'", true},
		{"rm -rf $'\\u002e\\u002e/other'", true},
		{"rm -rf $'\\U0000002e./other'", true},
		{"rm -rf $'../other\\x00junk'", true},
		{"rm -rf $'../other\\400junk'", true},
		{"rm -rf $'../other\\c@junk'", true},
		{"rm -rf $'../ot\\c junk'her", true},
		{"rm -rf $'../other\\c`junk'", true},
		{"rm -rf $'../other\\cࠀjunk'", true},
		{"rm -rf $'\\U80000000../other'", true},
		{"rm -rf $'../ot\\Uffffffffher'", true},
		{"rm -rf $'../other\\U80000001'", true},
		{"rm -rf $'../repo\\x00junk'tail", true},
		{"rm -rf $'../repo\\x'", true},
		{"rm -rf $'../repo\\c'z", true},
		{"echo x # rm -rf /var/tmp/crw-1028/TestWorktreeDelQuoteKeepsEarlierDenies3313292908/001/.codex/worktrees/zk3q", false},
		{"$'s\\x68' -c 'r\\\nm -rf ../../other'", true},
		{"su --command 'rm -rf ../other' root", true},
		{"su --session-command 'rm -rf ../other' root", true},
		{"su --com='rm -rf ../other' root", true},
		{"su --se 'rm -rf ../other' root", true},
		{"bash -c $'rm\\t-rf\\t../other'", true},
		{"eval $'rm\\t-rf\\t../other'", true},
		{"sh -c $'echo a\\nrm -rf ../other'", true},
		{"bash -c '<<<ignored; rm -rf ../other'", true},
		{"bash -oc posix 'rm -rf ../other'", true},
		{"bash --rcfile /dev/null -c 'rm -rf ../other'", true},
		{"bash --init-file /dev/null -c 'rm -rf ../other'", true},
		{"zsh -oshwordsplit -c 'rm -rf ../other'", true},
		{"su --shell /bin/sh root", true},
		{"bash script.sh", true},
		{"rm -rf '.\\\n' # sh", true},
		{"rm -rf ./build-zk3q", false},
		{"rm /var/tmp/crw-1028/TestFallbackForUnresolvableTargets613980038/001/.codex/worktrees/zk3q/repo", false},
		{"find . -name zk3q | xargs rm -rf", true},
		{"rm -rf \"$X\" && echo cleaning", true},
		{"rd /var/tmp/crw-1028/TestFallbackForUnresolvableTargets613980038/001/.codex/worktrees/zk3q/repo", false},
		{"Remove-Item -Recurse -LiteralPath /var/tmp/crw-1028/TestFallbackForUnresolvableTargets613980038/001/.codex/worktrees/zk3q", false},
		{"REMOVE-ITEM /var/tmp/crw-1028/TestFallbackForUnresolvableTargets613980038/001/.codex/worktrees/zk3q/repo", false},
		{"git worKtree remove /var/tmp/crw-1028/TestFallbackForUnresolvableTargets613980038/001/.codex/worktrees/zk3q/repo", true},
		{"cd ..\nrm -rf other", true},
		{"cat > clean.sh <<'EOF'\nrm -rf .\nEOF", false},
		{"echo \"rm -rf /var/tmp/crw-1028/TestExtendedWalkAddsDenies137562850/001/.codex/worktrees/zk3q\"", false},
		{"rm -rf ${TMPDIR}/x", true},
		{"source ./env.sh", true},
		{"bash script.sh <<EOF\n$X\nEOF", true},
		{"sh -c \"sh -c \\\"sh -c \\\\\\\"sh -c \\\\\\\\\\\\\\\"sh -c \\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"sh -c \\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"sh -c \\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"sh -c \\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"bash --version\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\\"\\\\\\\\\\\\\\\"\\\\\\\"\\\"\"", true},
		{"bash --rcfile \"$X\" script.sh", true},
		{"bash --init-file \"$X\" script.sh", true},
		{"Y=1 \"$GO\" test ./...", true},
		{"cat <<EOF; bash </dev/null\n$X\nEOF", true},
		{"bash script.sh \"$ARG\"", true},
		{"bash \"$SCRIPT\"", true},
		{"bash -c'rm -rf ../repo'", true},
		{"sh -c'rm -rf ../repo'", true},
		{"zsh -c'rm -rf ../repo'", true},
		{"su -sc'rm -rf ../repo' root", true},
		{"su -gc'rm -rf ../repo' root", true},
		{"su -c 'echo ok' root 'rm -rf ../repo'", true},
		{"su root -c 'echo ok' 'rm -rf ../repo'", true},
		{"su -lc'echo ok' root 'rm -rf ../repo'", true},
		{"su -s /bin/sh -c 'echo ok' root 'rm -rf ../repo'", true},
		{"ksh -c 'echo OK' 'rm -rf ../repo'", false},
		{"csh -c 'echo OK' 'rm -rf ../repo'", false},
		{"fish -c 'echo OK' 'rm -rf ../repo'", false},
		{"bash -c 'echo OK' <<< 'rm -rf ../repo'", false},
		{"bash -c 'echo OK' > 'log file' 'rm -rf ../repo'", false},
	}
}

// TestWorktreeDelChangedVerdicts checks that the reader gives each recorded command the recorded verdict.
func TestWorktreeDelChangedVerdicts(t *testing.T) {
	r := newDelRig(t)
	for _, c := range worktreeDelChangeRows() {
		if got := r.verdict(c.cmd); got.Deny != c.deny {
			t.Errorf("%s: deny=%v, recorded deny=%v (%s)", c.cmd, got.Deny, c.deny, got.Reason)
		}
	}
	r.intact(t)
}
