package hook

import (
	"strings"
	"testing"
)

// worktreeDelSpell reads a command written with <NL>, <BS>, <TAB> and <CR> for the bytes a test row cannot show.
func worktreeDelSpell(s string) string {
	return strings.NewReplacer("<NL>", "\n", "<BS>", "\\", "<TAB>", "\t", "<CR>", "\r").Replace(s)
}

// The extended walk removes a backslash-newline only where bash continues a line. The expected texts were checked
// against bash 5.3.9 through a stand-in command that prints its arguments, not derived from the scanner. An empty want
// means nothing is removed. A command with a here-document operator keeps the plain removal of every pair (a body is
// not shell text, and the scan cannot tell it from one).
func TestWorktreeDelJoinContinuations(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		// outside quotes: a backslash escapes the next byte, only backslash-newline goes
		{"rm -rf <BS><NL>.", "rm -rf ."},
		{"rm -rf .<BS><NL>{<BS><NL>cache,local}", "rm -rf .{cache,local}"},
		{"echo <BS><BS><NL>rm -rf ../repo", ""},
		{"echo <BS><BS><BS><NL>x", "echo <BS><BS>x"},
		{"echo a<BS>", ""},
		{"echo a <BS><CR><NL>d", ""},
		// a # that opens a word starts a comment, where nothing is removed
		{"echo a # c <BS><NL>rm -rf ../repo", ""},
		{"a<BS><NL>#b <BS><NL>c", "a#b c"},
		{"echo a <BS><NL># c <BS><NL>d", "echo a # c <BS><NL>d"},
		{"echo a<BS> <BS><NL>#b <BS><NL>c", "echo a<BS> #b c"},
		{"echo ''#b <BS><NL>c", "echo ''#b c"},
		{"echo '' #b <BS><NL>c", ""},
		{"# c <BS><NL>d", ""},
		{"(echo a)#c <BS><NL>rm -rf ../repo", ""},
		{"sleep 1&#c <BS><NL>d", ""},
		{"echo a<TAB>#c <BS><NL>d", ""},
		{"echo {#c <BS><NL>d", "echo {#c d"},
		{"echo $# <BS><NL>d", "echo $# d"},
		// single quotes keep everything; double quotes drop only the pair; $'...' keeps everything and \' does not end it
		{"rm -rf '.<BS><NL>'", ""},
		{"rm -rf \".<BS><NL>\"", "rm -rf \".\""},
		{"echo \"<BS><BS><NL>z\"", ""},
		{"echo \"a<BS>\"<BS><NL>b\"", "echo \"a<BS>\"b\""},
		{"rm -rf $'.<BS><NL>'", ""},
		{"echo $'it<BS>'s <BS><NL>x'", ""},
		{"echo 'a<BS><NL>b", ""},
		{"echo \"$'a<BS><NL>b'\"", "echo \"$'ab'\""},
		// a pair of dollars is the process id: only an odd run before a quote opens $'...' (also across a removed pair)
		{"rm -rf $<BS><NL>'.<BS><NL>'", "rm -rf $'.<BS><NL>'"},
		{"echo $$'a<BS>'; rm -rf .<BS><NL>; echo 'x' <BS>'", "echo $$'a<BS>'; rm -rf .; echo 'x' <BS>'"},
		{"echo <BS>$$'a<BS><NL>b'", ""},
		{"echo $$$'a<BS><NL>b'", ""},
		// a here-document operator, even one a continued line completes, keeps the plain removal
		{"echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>; # '", "echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo; # '"},
		{"cat <<EOF<NL>a<BS><NL>b<NL>EOF", "cat <<EOF<NL>ab<NL>EOF"},
		{"echo <BS><BS><NL>rm -rf ../repo <<EOF", "echo <BS>rm -rf ../repo <<EOF"},
		{"cat <<BS><NL><EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>", "cat <<EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo"},
	} {
		in, want := worktreeDelSpell(c.in), worktreeDelSpell(c.want)
		if c.want == "" {
			want = in
		}
		if got := worktreeDelJoinContinuations(in); got != want {
			t.Errorf("worktreeDelJoinContinuations(%q) = %q, want %q", in, got, want)
		}
	}
}

// The guard's verdicts for the continuation cases of the issue, each denied command next to an allowed neighbour. The
// first walk allows every denied row here: only the extended walk, and only a scanner that reads the line as bash does,
// denies it.
func TestWorktreeDelContinuationVerdicts(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ cmd, what string }{
		{"echo <BS><BS><NL>rm -rf ../repo", "rm -r ../repo"},                                // an escaped backslash: the newline ends the command
		{"echo a # c <BS><NL>rm -rf ../repo", "rm -r ../repo"},                              // a comment has no continuation
		{"(echo a)#c <BS><NL>rm -rf ../repo", "rm -r ../repo"},                              // a parenthesis ends a word
		{"rm -rf \".<BS><NL>\"", "rm -r ."},                                                 // double quotes continue the line
		{"echo $$'a<BS>'; rm -rf .<BS><NL>; echo 'x' <BS>'", "rm -r ."},                     // $$ is the process id, the quote after it is plain
		{"echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>; # '", "rm -r ../repo"}, // a here-document keeps the plain removal
		{"cat <<BS><NL><EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>", "rm -r ../repo"},
	} {
		r.denied(t, worktreeDelSpell(c.cmd), c.what)
	}
	for _, cmd := range []string{
		"rm -rf '.<BS><NL>'", "rm -rf $'.<BS><NL>'", // the pair is a part of the name
		"echo <BS><BS><BS><NL>rm -rf ../repo", // an escaped backslash, then a continued line: one echo
		"rm -rf .<BS><NL>/build", "rm -rf \".<BS><NL>/build\"",
		"echo <BS><BS><NL>rm -rf ./build", "echo a # c <BS><NL>rm -rf ./build", "(echo a)#c <BS><NL>rm -rf ./build",
		"echo $$'a<BS>'; rm -rf ./build<BS><NL>; echo 'x' <BS>'",
		"echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../other<BS><NL>; # '",
	} {
		r.allowed(t, worktreeDelSpell(cmd))
	}
	r.intact(t)
}
