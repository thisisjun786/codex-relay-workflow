package hook

import (
	"slices"
	"testing"
)

// The extended walk reads the quotes, backslashes and comments of a command as bash does. Each row was run in bash 5.3.9
// with a stand-in function for rm that prints its arguments: the denied command runs rm on the worktree itself (../repo,
// from the checkout) and its neighbour runs rm on ../other or ./build only. The first walk is the oracle's and allows
// every denied row that depends on quoting; only the extended walk can deny it.
func TestWorktreeDelQuoteVerdicts(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow string }{
		{"echo a # it's<NL>rm -rf ../repo", "echo a # it's<NL>rm -rf ../other"},         // a quote inside a comment opens nothing
		{"echo <BS>'<NL>rm -rf ../repo", "echo <BS>'<NL>rm -rf ../other"},               // an escaped single quote opens nothing
		{"echo <BS>\"<NL>rm -rf ../repo", "echo <BS>\"<NL>rm -rf ../other"},             // an escaped double quote opens nothing
		{"echo \"a<BS>\"b\"<NL>rm -rf ../repo", "echo \"a<BS>\"b\"<NL>rm -rf ../other"}, // an escaped quote does not close double quotes
		{"rm -rf ..<BS>/repo", "rm -rf ..<BS>/other"},                                   // quote removal: a backslash before a slash goes
		{"rm -rf <BS>.<BS>./repo", "rm -rf <BS>.<BS>./other"},
		{"rm -rf ../re<BS>po", "rm -rf ../ot<BS>her"},
		{"<BS>rm -rf ../repo", "<BS>rm -rf ../other"},                               // the escaped command word
		{"echo $'a<BS>'b'<NL>rm -rf ../repo", "echo $'a<BS>'b'<NL>rm -rf ../other"}, // an escaped quote does not end $'...'
		{"rm -rf $'../repo'", "rm -rf $'../other'"},                                 // $'...' and $"..." drop their dollar
		{"rm -rf $\"../repo\"", "rm -rf $\"../other\""},
		{"echo \"a<BS><BS>\"<NL>rm -rf ../repo", "echo \"a<BS><BS>\"<NL>rm -rf ../other"}, // an escaped backslash does not escape the closing quote
		{"echo a # c<NL>rm -rf ../repo", "echo a # c<NL>rm -rf ../other"},                 // the newline ends a comment and cuts
		{"echo a;# it's<NL>rm -rf ../repo", "echo a;# it's<NL>rm -rf ../other"},           // a # after each separator opens a comment
		{"echo a&# it's<NL>rm -rf ../repo", "echo a&# it's<NL>rm -rf ../other"},
		{"(echo a)# it's<NL>rm -rf ../repo", "(echo a)# it's<NL>rm -rf ../other"},
		{"{ # it's<NL>rm -rf ../repo; }", "{ # it's<NL>rm -rf ../other; }"},
		{"echo a # '; rm -rf ../other<NL>rm -rf ../repo", "echo a # '; rm -rf ../repo<NL>rm -rf ../other"}, // a comment hides the separators and quotes in it, and is not a command
		{"echo \"a # b\" & rm -rf ../repo", "echo \"a # b\" & rm -rf ./build"},                             // not comments: a # in double quotes
		{"echo '#' & rm -rf ../repo", "echo '#' & rm -rf ./build"},                                         // in single quotes
		{"echo ${x#y} & rm -rf ../repo", "echo ${x#y} & rm -rf ./build"},                                   // in a parameter expansion
		{"echo $# & rm -rf ../repo", "echo $# & rm -rf ./build"},                                           // after a dollar
		{"echo a#b & rm -rf ../repo", "echo a#b & rm -rf ./build"},                                         // inside a word
		{"echo <BS># & rm -rf ../repo", "echo <BS># & rm -rf ./build"},                                     // escaped
		{"echo ''# x & rm -rf ../repo", "echo ''# x & rm -rf ./build"},                                     // after a closed quote
	} {
		r.denied(t, worktreeDelSpell(c.deny), "rm -r ../repo")
		r.allowed(t, worktreeDelSpell(c.allow))
	}
	// Single quotes and double quotes keep a backslash that bash keeps: these name a path with a backslash in it, not the checkout.
	r.allowed(t, worktreeDelSpell("rm -rf \"../re<BS>po\""), worktreeDelSpell("rm -rf '..<BS>/repo'"), worktreeDelSpell("rm -rf \"..<BS><BS>/repo\""))
	r.intact(t)
}

// The first walk keeps the oracle's grammar, which knows quotes only: it reads no comment and no backslash, so it still allows the
// commands that the extended walk denies, and the guard's verdict for them comes from the extended walk alone.
func TestWorktreeDelQuoteFirstWalkIsTheOracles(t *testing.T) {
	r := newDelRig(t)
	for _, cmd := range []string{
		"echo a # it's<NL>rm -rf ../repo", "echo <BS>'<NL>rm -rf ../repo", "echo <BS>\"<NL>rm -rf ../repo", "echo \"a<BS>\"b\"<NL>rm -rf ../repo",
		"rm -rf ..<BS>/repo", "rm -rf <BS>.<BS>./repo", "rm -rf ../re<BS>po",
	} {
		cmd = worktreeDelSpell(cmd)
		if got := worktreeDelWalk(cmd, r.checkout, r.id(), false); got.Deny {
			t.Errorf("%q: the first walk denied (%s)", cmd, got.Reason)
		}
		r.denied(t, cmd, "rm -r ../repo")
	}
	if got, want := splitSegments(worktreeDelSpell("echo <BS>'<NL>rm -rf ../repo; echo a"), false), []string{worktreeDelSpell("echo <BS>'<NL>rm -rf ../repo; echo a")}; !slices.Equal(got, want) {
		t.Errorf("splitSegments(extended false) = %q, want the whole command as one segment", got)
	}
	if got, want := tokenize(worktreeDelSpell("rm -rf ..<BS>/repo")), []string{"rm", "-rf", worktreeDelSpell("..<BS>/repo")}; !slices.Equal(got, want) {
		t.Errorf("tokenize keeps the backslash: got %q, want %q", got, want)
	}
}
