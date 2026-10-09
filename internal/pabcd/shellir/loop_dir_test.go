package shellir

import "testing"

// TestLoopDirectoryChangeIsNotUndone: a directory change that a loop body makes, in the same statement as a command that only
// changes variables (arithmetic, let), stays a change. The next iteration starts in the changed directory, so a script the body
// runs after the change is judged in an unknown directory (CRW-1064 c1: real directory changes stay unknown).
func TestLoopDirectoryChangeIsNotUndone(t *testing.T) {
	for _, cmd := range []string{
		"for i in 1; do cd sub && (( i++ )); bash prog.sh; done",
		"for i in 1; do cd sub && let i++; bash prog.sh; done",
		"for i in 1; do cd sub && (( i++ )); done; bash prog.sh",
	} {
		if _, err := Analyze(cmd, "/work"); !isUnreadable(err) {
			t.Errorf("%q: err = %v, want unreadable", cmd, err)
		}
	}
}
