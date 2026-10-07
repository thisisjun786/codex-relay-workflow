package hook

import (
	"strings"
	"testing"
)

// CRW-772: a simple command whose name word the outer shell builds at run time is judged at every reading depth and in
// every program the guard reads. A name word that is wholly one command substitution or backtick pair is a command line
// the shell runs, so it is refused whatever its operands; a name word that carries a parameter expansion is judged as
// rm -r with the command's literal operands, so it is refused only when one of them reaches the protected worktree.
// CXC v0.2.40 allows every form: this is a security fix (port: fixed). The rows were checked in bash 5.3.9 with a
// harmless stand-in for rm (touch in a temporary directory); no row runs a deletion.

// worktreeDelNamedDenied asserts a deny whose reason names the command-name position, in the shape the issue fixes.
func worktreeDelNamedDenied(t *testing.T, r delRig, cmd, what string) {
	t.Helper()
	want := "[crw: WORKTREE-GUARD-03] blocked `a command the guard cannot name before it runs: " + what + "`: "
	if got := r.verdict(cmd); !got.Deny || !strings.HasPrefix(got.Reason, want) {
		t.Errorf("%q: got %+v, want a deny of %q", cmd, got, want)
	}
}

// TestWorktreeDelNamedByExpansionDenied is c9 and c11: the command-name rule refuses a name the outer shell builds, at
// every reading depth and in every program the guard reads.
func TestWorktreeDelNamedByExpansionDenied(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ command, what string }{
		{"$(printf 'rm -rf ../repo')", "a command line built by a command substitution"},
		{"`printf 'rm -rf ../repo'`", "a command line built by a command substitution"},
		{"X=rm; $X -rf ../repo", "a command named by an expansion"},
		{"\"$X\" -rf ../repo", "a command named by an expansion"},
		{"${X} -r ../repo", "a command named by an expansion"},
		{"bash -c '$X -rf ../repo'", "a command named by an expansion"},
		{"bash <<'EOF'\n$X -rf ../repo\nEOF", "a command named by an expansion"},
	} {
		worktreeDelNamedDenied(t, r, c.command, c.what)
	}
	r.intact(t)
}

// TestWorktreeDelNamedByExpansionAllowed is c11's allowed list: a parameter-expansion name whose operands reach nothing
// protected, and an assignment-only command, are read as today.
func TestWorktreeDelNamedByExpansionAllowed(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t,
		"\"$GO\" test ./...",
		"$EDITOR notes.md",
		"X=rm; echo $X",
		"\"$RELAY\" --state /tmp/x show",
	)
	r.intact(t)
}

// TestWorktreeDelNamedByExpansionHelper pins the two shapes directly: a name word that is one command substitution or
// backtick pair, and a name word that only carries a parameter expansion.
func TestWorktreeDelNamedByExpansionHelper(t *testing.T) {
	r := newDelRig(t)
	for _, c := range []struct{ segment, what string }{
		{"$(printf x)", "a command line built by a command substitution"},
		{"`printf x`", "a command line built by a command substitution"},
		{"\"$(printf x)\"", "a command line built by a command substitution"},
		{"$X -rf ../repo", "a command named by an expansion"},
		{"${X} -r ../repo", "a command named by an expansion"},
	} {
		got, ok := worktreeDelNamedByExpansion(c.segment, r.checkout, r.id(), true)
		if !ok || got != c.what {
			t.Errorf("worktreeDelNamedByExpansion(%q) = %q, %v; want %q, true", c.segment, got, ok, c.what)
		}
	}
	for _, segment := range []string{
		"$GO test ./...",
		"X=rm",
		"$X",
		"echo $X",
		"rm -rf ../repo",
	} {
		if got, ok := worktreeDelNamedByExpansion(segment, r.checkout, r.id(), true); ok {
			t.Errorf("worktreeDelNamedByExpansion(%q) = %q, true; want no refusal", segment, got)
		}
	}
}
