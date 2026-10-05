package hook

import (
	"slices"
	"strings"
	"testing"
)

// worktreeDelSpell reads a command written with <NL>, <BS>, <TAB>, <CR> and <BQ> for the bytes a test row cannot show.
func worktreeDelSpell(s string) string {
	return strings.NewReplacer("<NL>", "\n", "<BS>", "\\", "<TAB>", "\t", "<CR>", "\r", "<BQ>", "`").Replace(s)
}

// The extended walk removes a backslash-newline only where bash continues a line. The expected texts were checked
// against bash 5.3.9 through a stand-in command that prints its arguments, not derived from the scanner. An empty want
// means nothing is removed. A here-document body, a backtick body and a parameter expansion are read as the same flat
// text; TestWorktreeDelReadings covers the second reading a command with such a construct also gets.
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
		{"echo \"a\"#b <BS><NL>c", "echo \"a\"#b c"},
		{"echo '' #b <BS><NL>c", ""},
		{"echo a # c<NL># d <BS><NL>e", ""},                          // the newline ends a comment and a # at the start of the next line opens another
		{"echo a # c<NL>rm -rf <BS><NL>.", "echo a # c<NL>rm -rf ."}, // and the line after it continues as usual
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
		// a here-document or backtick command is scanned like any other, the body read as shell text
		{"echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>; # '", ""},
		{"cat <<EOF<NL>a<BS><NL>b<NL>EOF", "cat <<EOF<NL>ab<NL>EOF"},
		{"echo <BS><BS><NL>rm -rf ../repo <<EOF", ""},
		{"cat <<BS><NL><EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>", "cat <<EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>"},
		{"echo <BQ>echo safe; rm -rf '../repo<BS><NL>'; echo done<BQ>", ""},
		{"echo '<BQ>' <BS><NL>x", "echo '<BQ>' x"},
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

// A command with a here-document operator or a backtick in its plain-removal text is judged on two readings, the scan's and
// the plain removal of every pair, and denied when either denies; any other command has the scan reading only. An empty
// scan means the reading is the command itself, an empty plain means a single reading.
func TestWorktreeDelReadings(t *testing.T) {
	for _, c := range []struct{ in, scan, plain string }{
		{"echo <BQ>true<BQ>; echo <BS><BS><NL>rm -rf ../repo", "", "echo <BQ>true<BQ>; echo <BS>rm -rf ../repo"},
		{"echo <BS><BS><NL>rm -rf ../repo <<EOF<NL>x<NL>EOF", "", "echo <BS>rm -rf ../repo <<EOF<NL>x<NL>EOF"},
		{"echo a # <BQ> <BS><NL>rm -rf ../repo", "", "echo a # <BQ> rm -rf ../repo"},
		{"cat <<BS><NL><EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>", "cat <<EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>", "cat <<EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo"},
		{"echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>; # '", "", "echo <<'EOF'<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo; # '"},
		{"echo <BQ>echo safe; rm -rf '../repo<BS><NL>'; echo done<BQ>", "", "echo <BQ>echo safe; rm -rf '../repo'; echo done<BQ>"},
		{"echo '<BQ>'; echo ${v:-a # <BS><NL>rm -rf ../repo }", "", "echo '<BQ>'; echo ${v:-a # rm -rf ../repo }"}, // a # in a parameter expansion is not a comment: an extra block
		{"cat <<EOF<NL>a<BS><NL>b<NL>EOF", "cat <<EOF<NL>ab<NL>EOF", ""},                                           // both readings agree: one
		{"echo '<BQ>' <BS><NL>x", "echo '<BQ>' x", ""},
		{"echo <BS><BS><NL>rm -rf ../repo", "", ""}, // neither << nor a backtick: the scan reading only
		{"rm -rf '.<BS><NL>'", "", ""},
		{"cat <<EOF<NL>x<NL>EOF", "", ""}, // nothing to remove
	} {
		in, scan := worktreeDelSpell(c.in), worktreeDelSpell(c.scan)
		if c.scan == "" {
			scan = in
		}
		want := []string{scan}
		if c.plain != "" {
			want = append(want, worktreeDelSpell(c.plain))
		}
		if got := worktreeDelReadings(in); !slices.Equal(got, want) {
			t.Errorf("worktreeDelReadings(%q) = %q, want %q", in, got, want)
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
		{"echo <BQ>echo safe; rm -rf '../repo<BS><NL>'; echo done<BQ>", "rm -r ../repo"}, // bash removes the pair while it reads the backtick body
		{"echo <BQ>true<BQ>; echo <BS><BS><NL>rm -rf ../repo", "rm -r ../repo"},          // a backtick elsewhere does not bring the plain reading's bypass back
		{"echo <BS><BS><NL>rm -rf ../repo <<EOF<NL>x<NL>EOF", "rm -r ../repo"},
		{"echo a # <BQ> <BS><NL>rm -rf ../repo", "rm -r ../repo"},
		{"echo <BQ>x<BQ>; rm -rf '.<BS><NL>'", "rm -r ."}, // the plain reading's false block, kept as before this correction
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
		"echo <BQ>echo safe; rm -rf '../other<BS><NL>'; echo done<BQ>",
		"echo <BQ>true<BQ>; echo <BS><BS><NL>rm -rf ../other",
		"echo <BS><BS><NL>rm -rf ../other <<EOF<NL>x<NL>EOF",
		"echo a # <BQ> <BS><NL>rm -rf ../other",
		"echo <BQ>x<BQ>; rm -rf './build<BS><NL>'",                        // the plain reading's false block does not reach an unprotected target
		"cat <<BS><NL><EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../other<BS><NL>", // a here-document operator completed by a continued line
	} {
		r.allowed(t, worktreeDelSpell(cmd))
	}
	r.intact(t)
}
