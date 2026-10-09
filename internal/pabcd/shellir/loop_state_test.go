package shellir

import "testing"

// TestLoopKeepsDirectoryForHarmlessCalls: a loop whose body runs only harmless wrappers or functions leaves the directory the
// text set, so a script file after the loop is judged as it is before the loop (CRW-1064 c1).
func TestLoopKeepsDirectoryForHarmlessCalls(t *testing.T) {
	for _, cmd := range []string{
		"f() { :; }; for i in 1; do f; done; bash prog.sh",
		"for i in 1; do :; done; bash prog.sh",
		"for i in 1; do command true; done; bash prog.sh",
		"for i in 1; do builtin true; done; bash fd/0 </dev/null",
		"for i in 1; do command -v cd; done; bash prog.sh",
		"f() { n=1; }; for i in 1; do f; done; printf x | python3 stdin",
		"for i in 1; do command printf x; done; printf x | python3 fd/0",
		"f() { :; }; while builtin false; do f; done; printf x | python3 stdin",
		"f() { :; }; case x in x) f ;& y) command true ;; esac; printf x | python3 fd/0",
		"chdir sub; bash prog.sh",
	} {
		if _, err := Analyze(cmd, "/work"); err != nil {
			t.Errorf("%q: unreadable: %v", cmd, err)
		}
	}
}

// TestLoopMarksDirectoryChangeBehindCallsAsUnknown: a directory change in a loop body, reached through a function, a wrapper
// around cd or the zsh synonym chdir, leaves the directory unknown after the loop (CRW-1064 c1 and P2).
func TestLoopMarksDirectoryChangeBehindCallsAsUnknown(t *testing.T) {
	for _, cmd := range []string{
		"f() { cd sub; }; for i in 1; do f; done; bash prog.sh",
		"f() { f2; }; f2() { cd sub; }; for i in 1; do f; done; bash prog.sh",
		"for i in 1; do command cd sub; done; bash prog.sh",
		"for i in 1; do builtin cd sub; done; bash prog.sh",
		"for i in 1; do exec cd sub; done; bash prog.sh",
		"for i in 1; do chdir sub; done; bash prog.sh",
		"f() { chdir sub; }; for i in 1; do command f; done; bash prog.sh",
		"f() { CDPATH=sub; }; for i in 1; do f; done; bash prog.sh",
		"chdir ../repo; bash prog.sh",
	} {
		if _, err := Analyze(cmd, "/work"); err == nil {
			t.Errorf("%q: read, want unreadable", cmd)
		}
	}
}
