package shellir

import "testing"

// braceUnknown reports whether the reader fails to prove a command: a refused text, or a program or operand it cannot know.
func braceUnknown(cmd string) bool {
	res, err := Analyze(cmd, "/w")
	if err != nil {
		return true
	}
	for _, e := range res.Execs {
		if !e.Program.Known || anyUnknownArg(e.Args) {
			return true
		}
		for _, r := range e.Redirs {
			if !r.Target.Known {
				return true
			}
		}
	}
	return false
}

// TestBraceExpansionIsUnknown: an unquoted, unescaped brace expansion names a different word at run time, so the reader
// cannot know the word. Quoted parts inside the braces do not stop the expansion (bash expands {rm,"-rf"} to rm and -rf).
func TestBraceExpansionIsUnknown(t *testing.T) {
	for _, cmd := range []string{
		"{rm,echo} -rf ../repo",
		"{cp,install} /tmp/x out",
		"echo {1..3}",
		"echo a{,b}",
		"echo {a,{b,c}}",
		"{rm,\"-rf\"} x",
		"echo {a,'b'}",
		"echo {'a',b}",
		"echo {\"a\",b}",
		"echo {$x,b}",
		"echo {a,b}$x",
		"echo x{\"a\",b}y",
		"echo {1..\"3\"}",
		"echo {a,\\\"b\\\"}",
		"echo {\"a\",{b,c}}",
	} {
		if !braceUnknown(cmd) {
			t.Errorf("%q: every word known, want an unknown brace word", cmd)
		}
	}
}

// TestBraceControlsStayKnown: the spellings that are no brace expansion stay known words: quoted or escaped braces, a lone
// {}, a comma or a dot pair with no open brace, and a braced parameter expansion.
func TestBraceControlsStayKnown(t *testing.T) {
	for _, cmd := range []string{
		"echo '{a,b}'",
		"echo \"{a,b}\"",
		"echo \\{a,b\\}",
		"echo a\\,b",
		"echo {}",
		"echo {x}",
		"echo a,b",
		"echo '{a,'\"b\"'}'",
	} {
		if _, err := Analyze(cmd, "/w"); err != nil {
			t.Errorf("%q: refused (%v), want read", cmd, err)
		}
		if braceUnknown(cmd) {
			t.Errorf("%q: unknown, want known", cmd)
		}
	}
}

// TestBraceParameterExpansionIsNoGroup: a braced parameter expansion is one placeholder with no comma, so it is no group.
func TestBraceParameterExpansionIsNoGroup(t *testing.T) {
	if hasBraceExpansion("{P}") || hasBraceExpansion("P{P}P") {
		t.Errorf("a braced parameter expansion read as a brace group")
	}
	if !hasBraceExpansion("{P,P}") {
		t.Errorf("a group with a placeholder member is no group")
	}
}

func anyUnknownArg(args []Word) bool {
	for _, a := range args {
		if !a.Known {
			return true
		}
	}
	return false
}
