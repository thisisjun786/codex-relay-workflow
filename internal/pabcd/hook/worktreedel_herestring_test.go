package hook

import (
	"slices"
	"testing"
)

// CRW-657: a here-string written without a blank before its target, `bash -c 'source /dev/stdin' <<<'rm -rf ../repo'`, reached the
// reader as the one word `<<<rm -rf ../repo` and read as a command named `<<<rm`, so the deletion it fed to the shell was allowed
// (docs/port-cxc/known-defects.md, the CRW-639 section). The here-string operator ends its own word, exactly as when a blank
// stands between it and its target: `<<<'x'` is `<<<` and `x`. Only the third `<` of a plain `<<<` ends the word, so a here-document
// (`<<`), the other redirections and a quoted `'<<<'` read as before. Every row was checked against bash 5.3.9 with a stand-in
// `rm` that prints its arguments; the guard reads text and runs nothing.

// The words the bash-reading tokenizer makes of a here-string: the operator is its own word, with a single-quoted, double-quoted,
// $'...' or unquoted target after it, and the same words as when a blank stands between them. A here-document and a quoted <<<
// are not the operator.
func TestWorktreeDelHereStringSplit(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"<<<'rm -rf ../repo'", []string{"<<<", "rm -rf ../repo"}},
		{`<<<"rm -rf ../repo"`, []string{"<<<", "rm -rf ../repo"}},
		{"<<<$'rm -rf ../repo'", []string{"<<<", "rm -rf ../repo"}},
		{"<<<../repo", []string{"<<<", "../repo"}},
		{"x<<<'../repo'", []string{"x", "<<<", "../repo"}},
		{"2<<<'../repo'", []string{"2", "<<<", "../repo"}},
		// a blank already separates them, and the split must give the same words
		{"<<< 'rm -rf ../repo'", []string{"<<<", "rm -rf ../repo"}},
		{`<<< "rm -rf ../repo"`, []string{"<<<", "rm -rf ../repo"}},
		{"<<< $'rm -rf ../repo'", []string{"<<<", "rm -rf ../repo"}},
		{"<<< ../repo", []string{"<<<", "../repo"}},
		// a here-document operator, a quoted <<< and a lone operator are not split
		{"<<EOF", []string{"<<EOF"}},
		{"cat <<'EOF'", []string{"cat", "<<EOF"}},
		{"cat <<-EOF", []string{"cat", "<<-EOF"}},
		{"echo '<<<x'", []string{"echo", "<<<x"}},
		{`echo "<<<x"`, []string{"echo", "<<<x"}},
		{"<<<", []string{"<<<"}},
	} {
		if got := worktreeDelQuoteTokenize(c.in); !slices.Equal(got, c.want) {
			t.Errorf("worktreeDelQuoteTokenize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// The four attached-target rows the issue names are denied, and each is denied only by the quote-aware grammar: the oracle's
// first walk and the extended walk's old grammar allow it, so the guard's answer comes from the here-string split.
func TestWorktreeDelHereStringAttachedTargetDenied(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"bash -c 'source /dev/stdin' <<<'rm -rf ../repo'",
		"su -c 'source /dev/stdin' root <<<'rm -rf ../repo'",
		`bash -c 'source /dev/stdin' <<<"rm -rf ../repo"`,
		"bash <<<'rm -rf ../repo'",
		"bash -c 'source /dev/stdin' <<<$'rm -rf ../repo'",
	} {
		worktreeDelQuoteNeeds(t, r, cmd)
		r.denied(t, cmd, "rm -r ../repo")
	}
	r.intact(t)
}

// A here-string that feeds nothing dangerous stays allowed.
func TestWorktreeDelHereStringAllowed(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"cat <<<'hello'",
		"bash -c 'echo ok' <<<'x'",
		"cat <<<'rm -rf ../repo'",
		"echo <<<'rm -rf ../repo'",
	)
	r.intact(t)
}

// The split makes the blank-less command answer exactly as the command with a blank does, for every target form: this is what
// the issue asks for, and it is what pins the operator's own word.
func TestWorktreeDelHereStringAgreesWithTheBlank(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ attached, spaced string }{
		{"bash -c 'source /dev/stdin' <<<'rm -rf ../repo'", "bash -c 'source /dev/stdin' <<< 'rm -rf ../repo'"},
		{"su -c 'source /dev/stdin' root <<<'rm -rf ../repo'", "su -c 'source /dev/stdin' root <<< 'rm -rf ../repo'"},
		{`bash -c 'source /dev/stdin' <<<"rm -rf ../repo"`, `bash -c 'source /dev/stdin' <<< "rm -rf ../repo"`},
		{"bash -c 'source /dev/stdin' <<<$'rm -rf ../repo'", "bash -c 'source /dev/stdin' <<< $'rm -rf ../repo'"},
		{"bash <<<'rm -rf ../repo'", "bash <<< 'rm -rf ../repo'"},
		{"cat <<<'hello'", "cat <<< 'hello'"},
		{"bash -c 'echo ok' <<<'x'", "bash -c 'echo ok' <<< 'x'"},
		{"bash -c 'echo ok' <<<'rm -rf ../repo'", "bash -c 'echo ok' <<< 'rm -rf ../repo'"},
		{"bash -c 'source /dev/stdin' <<<'rm -rf ../other'", "bash -c 'source /dev/stdin' <<< 'rm -rf ../other'"},
	} {
		attached, spaced := r.verdict(c.attached), r.verdict(c.spaced)
		if attached.Deny != spaced.Deny || attached.Reason != spaced.Reason {
			t.Errorf("attached %q: %+v; spaced %q: %+v; want the same answer", c.attached, attached, c.spaced, spaced)
		}
	}
	r.intact(t)
}
