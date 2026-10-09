package shellir

import "testing"

func analyzeUnreadable(t *testing.T, cmd string, want bool) {
	t.Helper()
	_, err := Analyze(cmd, "/work")
	if got := err != nil; got != want {
		t.Errorf("%q: unreadable=%v, want %v (%v)", cmd, got, want, err)
	}
}

// TestInheritedStdinFileIsReplacedByAnInputRedirection: a shell inside a carried text whose own input redirection names a file the
// reader cannot name (a variable, a descriptor alias) reads that file, not the /dev/null the outer command gave it: the outer
// redirection is no proof of an empty program (CRW-1028 verifier finding 1).
func TestInheritedStdinFileIsReplacedByAnInputRedirection(t *testing.T) {
	for _, cmd := range []string{
		`bash -c 'bash < "$F"' </dev/null`,
		`bash -c 'bash < /dev/stdin' </dev/null`,
		`bash -c 'bash < /dev/fd/3' </dev/null`,
		`bash -c 'bash <&3' </dev/null`,
		`bash -c 'bash 0<&3' </dev/null`,
		`bash -c 'bash 0>&3' </dev/null`,
		`bash -c 'sh < "$F"' </dev/null`,
		`bash -c 'python3 < "$F"' </dev/null`,
		`bash -c 'node < "$F"' </dev/null`,
		`bash -c 'bash </dev/null < "$F"' </dev/null`,
		`bash -c 'bash < "$F" </dev/null; bash < "$F"' </dev/null`,
	} {
		analyzeUnreadable(t, cmd, true)
	}
	for _, cmd := range []string{
		`bash -c 'bash </dev/null' </dev/null`,
		`bash -c 'bash < run.sh' </dev/null`,
		`bash -c 'bash' </dev/null`,
		`bash -c 'bash < "$F" </dev/null' </dev/null`,
	} {
		analyzeUnreadable(t, cmd, false)
	}
}
