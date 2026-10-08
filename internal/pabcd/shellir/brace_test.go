package shellir

import "testing"

// TestBraceExpansionIsUnknown: an unquoted, unescaped brace expansion names a different word at run time, so a program
// position holding one is unreadable and an operand holding one is an unknown word. Quoted, escaped and lone braces are
// words the reader knows.
func TestBraceExpansionIsUnknown(t *testing.T) {
	for _, cmd := range []string{"{rm,echo} -rf ../repo", "{cp,install} /tmp/x out", "echo {1..3}", "echo a{,b}", "echo {a,{b,c}}"} {
		if _, err := Analyze(cmd, "/w"); err == nil {
			res, _ := Analyze(cmd, "/w")
			unknown := false
			for _, e := range res.Execs {
				if !e.Program.Known || anyUnknownArg(e.Args) {
					unknown = true
				}
			}
			if !unknown {
				t.Errorf("%q: every word known, want an unknown brace word", cmd)
			}
		}
	}
	for _, cmd := range []string{"echo '{a,b}'", "echo \"{a,b}\"", "echo \\{a,b\\}", "echo {}", "echo ${HOME}"} {
		if _, err := Analyze(cmd, "/w"); err != nil {
			t.Errorf("%q: refused (%v), want read", cmd, err)
		}
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
