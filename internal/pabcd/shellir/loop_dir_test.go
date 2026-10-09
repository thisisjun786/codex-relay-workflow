package shellir

import "testing"

// TestLoopDirectoryChangeIsNotUndone: a directory change that a loop body makes before a command that only changes variables
// (arithmetic, let) stays a change, so the directory after the loop is unknown (CRW-1064 c1, real directory changes stay unknown).
func TestLoopDirectoryChangeIsNotUndone(t *testing.T) {
	for _, cmd := range []string{
		"for i in 1; do cd sub && (( i++ )); done; bash prog.sh",
		"for i in 1; do cd sub && let i++; done; bash prog.sh",
	} {
		if _, err := Analyze(cmd, "/work"); !isUnreadable(err) {
			t.Errorf("%q: err = %v, want unreadable", cmd, err)
		}
	}
}
