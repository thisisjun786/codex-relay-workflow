package hook

import (
	"slices"
	"strings"
	"testing"
)

// worktreeDelQuoteGrammars says whether the old grammar of the extended walk and the quote-aware grammar each deny some reading of
// the command (the quote-aware one with and without the directory an eval moves).
func worktreeDelQuoteGrammars(r delRig, cmd string) (old, quoting bool) {
	for _, reading := range worktreeDelReadings(cmd) {
		old = old || worktreeDelQuoteWalk(reading, r.checkout, r.id(), true, false, 0).Deny
		for _, moves := range []bool{true, false} {
			verdict, _ := worktreeDelQuoteScan(reading, r.checkout, r.id(), true, true, moves, 0)
			quoting = quoting || verdict.Deny
		}
	}
	return old, quoting
}

// worktreeDelQuoteNeeds asserts that only the quote-aware grammar can deny cmd: the first walk and the old grammar allow it.
func worktreeDelQuoteNeeds(t *testing.T, r delRig, cmd string) {
	t.Helper()
	if got := worktreeDelWalk(cmd, r.checkout, r.id(), false); got.Deny {
		t.Errorf("%q: the first walk denied it (%s)", cmd, got.Reason)
	}
	if old, quoting := worktreeDelQuoteGrammars(r, cmd); old || !quoting {
		t.Errorf("%q: old grammar denies %v, quote-aware grammar denies %v; want false and true", cmd, old, quoting)
	}
}

// The extended walk reads the quotes, backslashes and comments of a command as bash does. Each row was run in bash 5.3.9
// with a stand-in function for rm that prints its arguments: the denied command runs rm on the worktree itself (../repo,
// from the checkout) and its neighbour runs rm on ../other or ./build only. The first walk is the oracle's and the old
// grammar of the extended walk read each of these wrongly: only the quote-aware grammar denies them.
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
		{"echo $'a<BS>'b'<NL>rm -rf ../repo", "echo $'a<BS>'b'<NL>rm -rf ../other"}, // an escaped quote does not end $'...'
		{"rm -rf $'../repo'", "rm -rf $'../other'"},                                 // $'...' and $"..." drop their dollar
		{"rm -rf $\"../repo\"", "rm -rf $\"../other\""},
		{"echo a;# it's<NL>rm -rf ../repo", "echo a;# it's<NL>rm -rf ../other"}, // a # after each separator opens a comment
		{"echo a&# it's<NL>rm -rf ../repo", "echo a&# it's<NL>rm -rf ../other"},
		{"(echo a)# it's<NL>rm -rf ../repo", "(echo a)# it's<NL>rm -rf ../other"},
		{"echo $(case x in x)# it's<NL>rm -rf ../repo ;; esac)", "echo $(case x in x)# it's<NL>rm -rf ../other ;; esac)"}, // a ) is a boundary also in a substitution: here a case pattern, and bash runs rm
		{"{ # it's<NL>rm -rf ../repo; }", "{ # it's<NL>rm -rf ../other; }"},
		{"echo a # '; rm -rf ../other<NL>rm -rf ../repo", "echo a # '; rm -rf ../repo<NL>rm -rf ../other"}, // a comment hides the separators and quotes in it, and is not a command
	} {
		deny := worktreeDelSpell(c.deny)
		worktreeDelQuoteNeeds(t, r, deny)
		r.denied(t, deny, "rm -r ../repo")
		r.allowed(t, worktreeDelSpell(c.allow))
	}
	// Single quotes and double quotes keep a backslash that bash keeps: these name a path with a backslash in it, not the checkout.
	r.allowed(t, worktreeDelSpell("rm -rf \"../re<BS>po\""), worktreeDelSpell("rm -rf '..<BS>/repo'"), worktreeDelSpell("rm -rf \"..<BS>/repo\""), worktreeDelSpell("rm -rf \"..<BS><BS>/repo\""))
	r.intact(t)
}

// Commands that the old grammar already denied, and that the quote-aware grammar must deny by itself: a # in double quotes, in a
// parameter expansion, after a dollar, inside a word, escaped or after a closed quote is no comment (the denied row would
// vanish with it), and an escaped backslash does not escape the quote after it.
func TestWorktreeDelQuoteAgrees(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow string }{
		{"echo \"a<BS><BS>\"<NL>rm -rf ../repo", "echo \"a<BS><BS>\"<NL>rm -rf ../other"}, // an escaped backslash does not escape the closing quote
		{"echo a # c<NL>rm -rf ../repo", "echo a # c<NL>rm -rf ../other"},                 // the newline ends a comment and cuts
		{"echo \"a # b\" & rm -rf ../repo", "echo \"a # b\" & rm -rf ./build"},            // not comments: a # in double quotes
		{"echo '#' & rm -rf ../repo", "echo '#' & rm -rf ./build"},                        // in single quotes
		{"echo ${x#y} & rm -rf ../repo", "echo ${x#y} & rm -rf ./build"},                  // in a parameter expansion
		{"echo $# & rm -rf ../repo", "echo $# & rm -rf ./build"},                          // after a dollar
		{"echo a#b & rm -rf ../repo", "echo a#b & rm -rf ./build"},                        // inside a word
		{"echo <BS># & rm -rf ../repo", "echo <BS># & rm -rf ./build"},                    // escaped
		{"echo ''# x & rm -rf ../repo", "echo ''# x & rm -rf ./build"},                    // after a closed quote
		{"echo <BQ>x<BQ># & rm -rf ../repo", "echo <BQ>x<BQ># & rm -rf ../other"},         // after a closing backtick
	} {
		deny := worktreeDelSpell(c.deny)
		r.denied(t, deny, "rm -r ../repo")
		r.allowed(t, worktreeDelSpell(c.allow))
		if _, quoting := worktreeDelQuoteGrammars(r, deny); !quoting {
			t.Errorf("%q: the quote-aware grammar allowed it", deny)
		}
	}
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

// Verdicts for the forms that reach the guard by other paths: a leading comment, the escaped command word, a cd and a git -C
// whose directory is spelled with backslashes, the numeric and control escapes of $'...' (a NUL ends the string, as bash ends
// it), an opening backtick (a # after it is a comment, a # after a closing one is not) and a brace that is an argument. Each
// row was run in bash 5.3.9 with a stand-in rm function, the denied ones run rm on ../repo (or on repo after the cd), the
// first walk allows each of them, and each neighbour (../other) is allowed.
func TestWorktreeDelQuoteTargets(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow, what string }{
		{"# it's<NL>rm -rf ../repo", "# it's<NL>rm -rf ../other", "rm -r ../repo"},
		{"r<BS>m -rf ../repo", "r<BS>m -rf ../other", "rm -r ../repo"},
		{"cd <BS>.<BS>.; rm -rf repo", "cd <BS>.<BS>.; rm -rf other", "rm -r repo"},
		{"git -C <BS>.<BS>. worktree remove repo", "git -C <BS>.<BS>. worktree remove other", "git worktree remove repo"},
		{"rm -rf $'<BS>x2e<BS>x2e/repo'", "rm -rf $'<BS>x2e<BS>x2e/other'", "rm -r ../repo"},
		{"rm -rf $'<BS>056<BS>056/repo'", "rm -rf $'<BS>056<BS>056/other'", "rm -r ../repo"},
		{"rm -rf $'<BS>u002e<BS>u002e/repo'", "rm -rf $'<BS>u002e<BS>u002e/other'", "rm -r ../repo"},
		{"rm -rf $'<BS>U0000002e./repo'", "rm -rf $'<BS>U0000002e./other'", "rm -r ../repo"},
		{"rm -rf $'../repo<BS>x00junk'", "rm -rf $'../other<BS>x00junk'", "rm -r ../repo"},
		{"rm -rf $'../repo<BS>400junk'", "rm -rf $'../other<BS>400junk'", "rm -r ../repo"},
		{"rm -rf $'../repo<BS>c@junk'", "rm -rf $'../other<BS>c@junk'", "rm -r ../repo"},
		{"rm -rf $'../re<BS>c junk'po", "rm -rf $'../ot<BS>c junk'her", "rm -r ../repo"},
		{"rm -rf $'../repo<BS>c<BQ>junk'", "rm -rf $'../other<BS>c<BQ>junk'", "rm -r ../repo"},
		{"rm -rf $'../repo<BS>c\u0800junk'", "rm -rf $'../other<BS>c\u0800junk'", "rm -r ../repo"},
		{"rm -rf $'<BS>U80000000../repo'", "rm -rf $'<BS>U80000000../other'", "rm -r ../repo"},
		{"rm -rf $'../re<BS>Uffffffffpo'", "rm -rf $'../ot<BS>Uffffffffher'", "rm -r ../repo"},
		{"rm -rf $'../repo<BS>U80000001'", "rm -rf $'../other<BS>U80000001'", "rm -r ../repo"},
		{"echo <BQ>#it's<BQ><NL>rm -rf ../repo", "echo <BQ>#it's<BQ><NL>rm -rf ../other", "rm -r ../repo"},
		{"echo <BQ>#x'<BQ>; rm -rf ../repo", "echo <BQ>#x'<BQ>; rm -rf ../other", "rm -r ../repo"},
		{"echo <BQ>#x'<BS><BS><BQ>; rm -rf ../repo", "echo <BQ>#x'<BS><BS><BQ>; rm -rf ../other", "rm -r ../repo"}, // an even run of backslashes does not escape the backtick
		{"rm -rf { ..<BS>/repo", "rm -rf { ..<BS>/other", "rm -r ../repo"},
		{"rm -rf } ..<BS>/repo", "rm -rf } ..<BS>/other", "rm -r ../repo"},
		{"rm { -rf ..<BS>/repo", "rm { -rf ..<BS>/other", "rm -r ../repo"},
		{"{ rm -rf ..<BS>/repo; }", "{ rm -rf ..<BS>/other; }", "rm -r ../repo"},
		{"if true; then { rm -rf ..<BS>/repo; }; fi", "if true; then { rm -rf ..<BS>/other; }; fi", "rm -r ../repo"},
		{"function f { rm -rf ..<BS>/repo; }; f", "function f { rm -rf ..<BS>/other; }; f", "rm -r ../repo"},
		{"coproc { rm -rf ..<BS>/repo; }", "coproc { rm -rf ..<BS>/other; }", "rm -r ../repo"},
		{"time -p { rm -rf ..<BS>/repo; }", "time -p { rm -rf ..<BS>/other; }", "rm -r ../repo"},
		{"echo <BQ>{ rm -rf ..<BS>/repo; }<BQ>", "echo <BQ>{ rm -rf ..<BS>/other; }<BQ>", "rm -r ../repo"},
		{"rm -rf 2>&1 ..<BS>/repo", "rm -rf 2>&1 ..<BS>/other", "rm -r ../repo"},
		{"rm -rf ..<BS>/repo>/dev/null", "rm -rf ..<BS>/other>/dev/null", "rm -r ../repo"},
		{"rm -rf ..<BS>/repo &>/dev/null", "rm -rf ..<BS>/other &>/dev/null", "rm -r ../repo"},
		{"rm -rf ..<BS>/repo >&2", "rm -rf ..<BS>/other >&2", "rm -r ../repo"},
		{"rm -rf ..<BS>/repo<<EOF<NL>x<NL>EOF", "rm -rf ..<BS>/other<<EOF<NL>x<NL>EOF", "rm -r ../repo"},
	} {
		deny := worktreeDelSpell(c.deny)
		worktreeDelQuoteNeeds(t, r, deny)
		r.denied(t, deny, c.what)
		r.allowed(t, worktreeDelSpell(c.allow))
	}
	// Where the text after a closing quote joins the word, or an escape has no digits, the word is another path.
	r.allowed(t, worktreeDelSpell("rm -rf $'../repo<BS>x00junk'tail"), worktreeDelSpell("rm -rf $'../repo<BS>x'"), worktreeDelSpell("rm -rf $'../repo<BS>c'z"))
	r.intact(t)
}

// The extended walk judges every reading over the old grammar first and over bash's reading second, so nothing that the old
// grammar denied is allowed and each earlier deny keeps its reason. A comment that holds the slot id and a verb arms the old
// grammar's fallback, as it did before (the hidden verb r\m after it is unresolved). The quote-aware grammar reads the comment
// as text, so a deny that rests on a comment alone comes from the old grammar and the first walk.
func TestWorktreeDelQuoteKeepsEarlierDenies(t *testing.T) {
	r := newDelRig(t)
	r.denied(t, worktreeDelSpell("X=../repo<NL>echo a<NL># <BQ>rm zk3q<NL>r<BS>m -rf $X"), unresolvable)
	r.denied(t, "echo x # rm -rf "+r.slotRoot, unresolvable)
	if got := worktreeDelQuoteWalk("echo x # rm -rf "+r.slotRoot, r.checkout, r.id(), true, true, 0); got.Deny {
		t.Errorf("the quote-aware grammar read a comment as a command: %s", got.Reason)
	}
	// A here-document body is data to bash and shell text to both grammars: the old grammar reads this body's quote as closed.
	r.denied(t, worktreeDelSpell("cat <<EOF<NL>$'x<BS>'<NL>EOF<NL>rm -rf ../repo<BS><NL>; # '"), "rm -r ../repo")
}

// The extended splitter cuts a command where bash ends a command: at ; | & and a newline in plain state, at a group brace and
// at a parenthesis; a backslash pair, quotes and a comment hide the rest, and a comment is dropped.
func TestWorktreeDelQuoteSegments(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"echo a # it's<NL>rm x", []string{"echo a", "rm x"}},
		{"echo <BS>'<NL>rm x", []string{"echo <BS>'", "rm x"}},
		{"echo \"a<BS>\"b\"<NL>rm x", []string{"echo \"a<BS>\"b\"", "rm x"}},
		{"echo $'a<BS>'b'; rm x", []string{"echo $'a<BS>'b'", "rm x"}},
		{"echo a<BS>; rm x", []string{"echo a<BS>; rm x"}},
		{"echo a # c ; ( { <NL>rm x", []string{"echo a", "rm x"}},
		{"echo a # c <BS><NL>rm x", []string{"echo a", "rm x"}},
		{"# it's<NL>rm x", []string{"rm x"}},
		{"echo a#b; rm x", []string{"echo a#b", "rm x"}},
		{"echo \"a # b\"; rm x", []string{"echo \"a # b\"", "rm x"}},
		{"echo <BQ>#it's<BQ><NL>rm x", []string{"echo <BQ><BQ>", "rm x"}},
		{"echo <BQ>#x'<BQ>; rm x", []string{"echo <BQ><BQ>", "rm x"}}, // the comment ends at the closing backtick
		{"echo <BQ>x<BQ># ; rm x", []string{"echo <BQ>x<BQ>#", "rm x"}},
		{"rm -rf { x", []string{"rm -rf { x"}},
		{"{ rm x; }", []string{"rm x"}},
		{"then { rm x; }", []string{"then", "rm x"}},
		{"echo <BS> { rm x; }", []string{"echo <BS> { rm x"}},
		{"echo { rm x; }", []string{"echo", "rm x"}},
		{"rm { x; }", []string{"rm { x"}},
		{"rm -rf 2>&1 x", []string{"rm -rf 2>&1 x"}},
		{"a &>f; b >&2", []string{"a &>f", "b >&2"}},
		{"a & >f", []string{"a", ">f"}},
		// the rows of TestSplitSegmentsExtended, which both grammars cut alike
		{"cd /tmp<NL>rm x", []string{"cd /tmp", "rm x"}},
		{"sleep 1 & rm x && ls", []string{"sleep 1", "rm x", "ls"}},
		{"(cd /tmp); rm x", []string{"(", "cd /tmp", ")", "rm x"}},
		{"echo $(rm x)", []string{"echo $", "(", "rm x", ")"}},
		{"a;{ rm x; }", []string{"a", "rm x"}},
		{"rm -rf .{cache,local} ${HOME}/x {a,b}", []string{"rm -rf .{cache,local} ${HOME}/x {a,b}"}},
		{"echo 'a<NL>b (c)' && ls", []string{"echo 'a<NL>b (c)'", "ls"}},
		{"a || b | c", []string{"a", "b", "c"}},
	} {
		in := worktreeDelSpell(c.in)
		want := make([]string, len(c.want))
		for i, s := range c.want {
			want[i] = worktreeDelSpell(s)
		}
		if got := worktreeDelQuoteSegments(in); !slices.Equal(got, want) {
			t.Errorf("worktreeDelQuoteSegments(%q) = %q, want %q", in, got, want)
		}
	}
}

// The extended tokenizer removes quotes and escaping backslashes as bash does. The expected words are the arguments that bash
// passed to a stand-in rm function (bash 5.3.9) for the command rm followed by the text of the row; a NUL that an escape in
// $'...' makes ends that string, and the text after its closing quote joins the word.
func TestWorktreeDelQuoteTokenize(t *testing.T) {
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"..<BS>/repo", []string{"../repo"}},
		{"<BS>.<BS>./repo", []string{"../repo"}},
		{"a<BS> b", []string{"a b"}},
		{"\"a<BS>\"b\"", []string{"a\"b"}},
		{"\"a<BS>b\"", []string{"a<BS>b"}},
		{"\"a<BS><BS>b\"", []string{"a<BS>b"}},
		{"\"<BS>$x<BS><BQ>\"", []string{"$x<BQ>"}},
		{"'a<BS>b'", []string{"a<BS>b"}},
		{"$'a<BS>'b'", []string{"a'b"}},
		{"$\"x y\"", []string{"x y"}},
		{"$'<BS>x2e<BS>x2e/repo'", []string{"../repo"}},
		{"$'<BS>056<BS>056/repo'", []string{"../repo"}},
		{"$'<BS>u002e<BS>u002e/repo'", []string{"../repo"}},
		{"$'<BS>U0000002e./repo'", []string{"../repo"}},
		{"$'../repo<BS>x00junk'tail", []string{"../repotail"}},
		{"$'../repo<BS>0junk'tail", []string{"../repotail"}},
		{"$'../repo<BS>c@junk'tail", []string{"../repotail"}},
		{"$'../repo<BS>c junk'tail", []string{"../repotail"}},
		{"$'../repo<BS>c<BQ>junk'tail", []string{"../repotail"}},
		{"$'../repo<BS>c\u0800junk'tail", []string{"../repotail"}},
		{"$'<BS>x'", []string{"<BS>x"}},
		{"$'<BS>x2eg'", []string{".g"}},
		{"$'<BS>1234'", []string{"S4"}},
		{"$'<BS>8'", []string{"<BS>8"}},
		{"$'<BS>q'", []string{"<BS>q"}},
		{"$'<BS>?<BS><BS>'", []string{"?<BS>"}},
		{"$'<BS>400'tail", []string{"tail"}},
		{"$'<BS>u'", []string{"<BS>u"}},
		{"$'<BS>a<BS>b<BS>f<BS>n<BS>r<BS>t<BS>v<BS>e<BS>E'", []string{"\a\b\f\n\r\t\v\x1b\x1b"}},
		{"$'<BS>U80000000../repo'", []string{"../repo"}},
		{"$'../re<BS>Uffffffffpo'", []string{"../repo"}},
		{"''", []string{""}},
		{"a''b", []string{"ab"}},
		{"$''", []string{""}},
		{"x<BS>$'y'", []string{"x$y"}},
		{"'a'<BS>'b", []string{"a'b"}},
		{"\"<BS>\"\"", []string{"\""}},
		{"a$'b'c", []string{"abc"}},
		{"<BS>#a", []string{"#a"}},
		{"a\"b c\"d e", []string{"ab cd", "e"}},
		{"\"<BS>a<BS>n<BS>x\"", []string{"<BS>a<BS>n<BS>x"}},
		{"\"<BS>/<BS>.\"", []string{"<BS>/<BS>."}},
		{"$'a<BS>c<BS><BS>z'", []string{"a\x1cz"}},
		{"$'a<BS>c<BS>'z'", []string{"a\x1c'z"}},
		{"$'a<BS>c'z", []string{"a<BS>cz"}},
		{"$'a<BS>cA'z", []string{"a\x01z"}},
		{"$'../repo<BS>c?junk'", []string{"../repo\x7fjunk"}},
	} {
		in := worktreeDelSpell("rm " + c.in)
		want := []string{"rm"}
		for _, w := range c.want {
			want = append(want, worktreeDelSpell(w))
		}
		if got := worktreeDelQuoteTokenize(in); !slices.Equal(got, want) {
			t.Errorf("worktreeDelQuoteTokenize(%q) = %q, want %q", in, got, want)
		}
	}
	// JavaScript's whitespace separates words in plain state, as in the oracle's tokenizer; an escaped one does not (as in bash).
	for _, c := range []struct {
		in   string
		want []string
	}{
		{"a\u00a0b", []string{"rm", "a", "b"}},
		{"a<BS>\u00a0b", []string{"rm", "a\u00a0b"}},
		{"'a\u00a0b'", []string{"rm", "a\u00a0b"}},
		// a redirection operator ends the word before it, in plain state only
		{"a>b", []string{"rm", "a", ">b"}},
		{"a 2>&1", []string{"rm", "a", "2", ">&1"}},
		{"a<<EOF", []string{"rm", "a", "<<EOF"}},
		{"'a>b'", []string{"rm", "a>b"}},
		{"a<BS>>b", []string{"rm", "a>b"}},
	} {
		in := worktreeDelSpell("rm " + c.in)
		if got := worktreeDelQuoteTokenize(in); !slices.Equal(got, c.want) {
			t.Errorf("worktreeDelQuoteTokenize(%q) = %q, want %q", in, got, c.want)
		}
	}
}

// CRW-585 regression (pair evaluation of PR #557): a program string that a shell reads again has its backslash-newline pairs
// removed by that shell even inside this shell's single quotes, so sh -c 'r<backslash><newline>m -rf ../../zk3q' runs rm on
// the slot. Before CRW-585 every pair was removed and the guard denied these through its fallback; with the scan reading alone
// the pair kept r and m apart. The extended walk now judges the program string as a command of its own. Each row ran rm in bash
// 5.3.9 (and dash for sh) with a stand-in rm script; the neighbours name ../../other, and the last rows are commands that name a
// shell word without handing it a program, where bash runs rm on the literal name.
func TestWorktreeDelQuoteShellProgram(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ deny, allow, what string }{
		{"sh -c 'r<BS><NL>m -rf ../../zk3q'", "sh -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"bash -c 'r<BS><NL>m -rf ../../zk3q'", "bash -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"eval 'r<BS><NL>m -rf ../../zk3q'", "eval 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"bash -x -c 'r<BS><NL>m -rf ../../zk3q'", "bash -x -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"sh -ec 'r<BS><NL>m -rf ../../zk3q'", "sh -ec 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"s<BS>h -c 'r<BS><NL>m -rf ../../zk3q'", "s<BS>h -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"s'h' -c 'r<BS><NL>m -rf ../../zk3q'", "s'h' -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"e<BS>val 'r<BS><NL>m -rf ../../zk3q'", "e<BS>val 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"/bin/s<BS>h -c 'r<BS><NL>m -rf ../../zk3q'", "/bin/s<BS>h -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"$'s<BS>x68' -c 'r<BS><NL>m -rf ../../zk3q'", "$'s<BS>x68' -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"env sh -c 'r<BS><NL>m -rf ../../zk3q'", "env sh -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"nohup bash -c 'r<BS><NL>m -rf ../../zk3q'", "nohup bash -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"xargs sh -c 'r<BS><NL>m -rf ../../zk3q'", "xargs sh -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"su -c 'r<BS><NL>m -rf ../../zk3q'", "su -c 'r<BS><NL>m -rf ../../other'", "rm -r ../../zk3q"},
		{"su --command 'rm -rf ../repo' root", "su --command 'rm -rf ../other' root", "rm -r ../repo"}, // su's long options (util-linux 2.41 su --help)
		{"su --session-command 'rm -rf ../repo' root", "su --session-command 'rm -rf ../other' root", "rm -r ../repo"},
		{"su --com='rm -rf ../repo' root", "su --com='rm -rf ../other' root", "rm -r ../repo"}, // an abbreviation with its argument attached
		{"su --se 'rm -rf ../repo' root", "su --se 'rm -rf ../other' root", "rm -r ../repo"},
		{"bash -c 2>/dev/null 'rm -rf ../repo'", "bash -c 2>/dev/null 'rm -rf ../other'", "rm -r ../repo"},
		{"bash -c -e 'rm -rf ../repo'", "bash -c -e 'rm -rf ../other'", "rm -r ../repo"},
		{"bash -c -- 'rm -rf ../repo'", "bash -c -- 'rm -rf ../other'", "rm -r ../repo"},
		{"eval -- 'rm -rf ../repo'", "eval -- 'rm -rf ../other'", "rm -r ../repo"},
		{"bash -c $'rm<BS>t-rf<BS>t../repo'", "bash -c $'rm<BS>t-rf<BS>t../other'", "rm -r ../repo"}, // a control escape separates the words
		{"eval $'rm<BS>t-rf<BS>t../repo'", "eval $'rm<BS>t-rf<BS>t../other'", "rm -r ../repo"},
		{"sh -c $'echo a<BS>nrm -rf ../repo'", "sh -c $'echo a<BS>nrm -rf ../other'", "rm -r ../repo"},
		{"sudo -u root bash -c 'rm -rf ../repo'", "sudo -u root bash -c 'rm -rf ../other'", "rm -r ../repo"},
		{"timeout 5 sh -c 'rm -rf ../repo'", "timeout 5 sh -c 'rm -rf ../other'", "rm -r ../repo"},
		{"eval 'cd ..'; rm -rf repo", "eval 'cd ..'; rm -rf other", "rm -r repo"},                          // eval runs in this shell: its cd moves the later rm
		{"eval '(cd ..)'; r<BS>m -rf ../repo", "eval '(cd ..)'; r<BS>m -rf ../other", "rm -r ../repo"},     // but not a cd in a subshell
		{"bash -c '<<<ignored; rm -rf ../repo'", "bash -c '<<<ignored; rm -rf ../other'", "rm -r ../repo"}, // a quoted program that starts like a redirection
		{"timeout 5s bash -c 'rm -rf ../repo'", "timeout 5s bash -c 'rm -rf ../other'", "rm -r ../repo"},
		{"timeout 0.5 sh -c 'rm -rf ../repo'", "timeout 0.5 sh -c 'rm -rf ../other'", "rm -r ../repo"},
		{"bash -c -O nullglob 'rm -rf ../repo'", "bash -c -O nullglob 'rm -rf ../other'", "rm -r ../repo"}, // an option with an argument
		{"bash -co posix 'rm -rf ../repo'", "bash -co posix 'rm -rf ../other'", "rm -r ../repo"},           // the argument of an o inside a cluster is not the program
		{"bash -oc posix 'rm -rf ../repo'", "bash -oc posix 'rm -rf ../other'", "rm -r ../repo"},
		{"bash -cO nullglob 'rm -rf ../repo'", "bash -cO nullglob 'rm -rf ../other'", "rm -r ../repo"},
		{"bash +c 'rm -rf ../repo'", "bash +c 'rm -rf ../other'", "rm -r ../repo"},                           // bash takes +c for -c
		{"bash -c -- '-missing; rm -rf ../repo'", "bash -c -- '-missing; rm -rf ../other'", "rm -r ../repo"}, // nothing after -- is an option
		{"eval -- '+echo; rm -rf ../repo'", "eval -- '+echo; rm -rf ../other'", "rm -r ../repo"},
		{"eval -- '-missing; rm -rf ../repo'", "eval -- '-missing; rm -rf ../other'", "rm -r ../repo"},
		{"bash --rcfile /dev/null -c 'rm -rf ../repo'", "bash --rcfile /dev/null -c 'rm -rf ../other'", "rm -r ../repo"}, // a long option with an argument
		{"bash --init-file /dev/null -c 'rm -rf ../repo'", "bash --init-file /dev/null -c 'rm -rf ../other'", "rm -r ../repo"},
		{"bash --norc -c 'rm -rf ../repo'", "bash --norc -c 'rm -rf ../other'", "rm -r ../repo"}, // a long option without an argument
		{"eval '+echo; rm -rf ../repo'", "eval '+echo; rm -rf ../other'", "rm -r ../repo"},       // eval has no + options
		{"bash -n +n -c 'rm -rf ../repo'", "bash -n +n -c 'rm -rf ../other'", "rm -r ../repo"},
		{"eval \"eval 'cd ..'\"; rm -rf repo", "eval \"eval 'cd ..'\"; rm -rf other", "rm -r repo"},            // the cd of a nested eval moves this shell too
		{"eval 'cd .. | cat'; r<BS>m -rf ../repo", "eval 'cd .. | cat'; r<BS>m -rf ../other", "rm -r ../repo"}, // a cd in a pipeline or a background job does not
		{"eval 'cd .. & wait'; r<BS>m -rf ../repo", "eval 'cd .. & wait'; r<BS>m -rf ../other", "rm -r ../repo"},
		{"bash -c 'rm -rf .'", "bash -c 'rm -rf ./build'", "rm -r ."}, // a program string with a relative target, found by the same reading
	} {
		deny := worktreeDelSpell(c.deny)
		worktreeDelQuoteNeeds(t, r, deny)
		r.denied(t, deny, c.what)
		r.allowed(t, worktreeDelSpell(c.allow))
	}
	// This shell removes the pair in double quotes, so the guard denied it before and after CRW-585 (through the fallback).
	r.denied(t, worktreeDelSpell("sh -c \"r<BS><NL>m -rf ../../zk3q\""), unresolvable)
	// sh -c runs in a shell of its own: its cd does not move this one. Words that only mention a shell are text, not a command.
	r.allowed(t, worktreeDelSpell("sh -c 'cd ..'; rm -rf repo"), worktreeDelSpell("eval '(cd ..)'; rm -rf repo"), "echo sh -c 'rm -rf .'", "printf '%s' sh -c 'rm -rf .'", "echo bash -c 'rm -rf ../repo'", "echo eval 'rm -rf ../repo'",
		"echo su --command 'rm -rf ../repo'", "su --shell /bin/sh root",
		"bash -n -c 'rm -rf ../repo'", "bash script.sh -c 'rm -rf ../repo'", "bash - -c 'rm -rf ../repo'") // a syntax check, and -c as an argument of a script or a script's name
	// Without a program string handed to a shell the pair stays a part of a name, as CRW-585 reads it.
	r.allowed(t, worktreeDelSpell("echo 'r<BS><NL>m -rf ../../zk3q'"), worktreeDelSpell("rm -rf '.<BS><NL>' sh"), worktreeDelSpell("rm -rf '.<BS><NL>' # sh"), worktreeDelSpell("rm -rf '.<BS><NL>'"))
	r.intact(t)
}

// worktreeDelQuoteNest hands the program to sh -c in double quotes, depth times.
func worktreeDelQuoteNest(program string, depth int) string {
	for range depth {
		program = "sh -c \"" + strings.NewReplacer("\\", "\\\\", "\"", "\\\"").Replace(program) + "\""
	}
	return program
}

// A program string inside a program string is judged down to worktreeDelQuoteDepth levels, and a deeper nest costs no more than
// that: the walk stops following programs there (the fallback of the old grammar still sees the text). The text doubles its
// backslashes at each level, so twelve levels are as deep as a test needs.
func TestWorktreeDelQuoteNestedPrograms(t *testing.T) {
	r := newDelRig(t)
	worktreeDelQuoteNeeds(t, r, worktreeDelQuoteNest("rm -rf .", 3))
	r.denied(t, worktreeDelQuoteNest("rm -rf .", 3), "rm -r .")
	r.allowed(t, worktreeDelQuoteNest("rm -rf ./build", 3), worktreeDelQuoteNest("rm -rf ./build", 12))
	if got := r.verdict(worktreeDelQuoteNest("rm -rf .", 12)); got.Deny {
		t.Errorf("a nest past the depth limit was followed: %s", got.Reason)
	}
}
