package ownership

import "testing"

// Normpath is posixpath.normpath: Python's own answers for each shape, notably exactly two leading
// slashes kept where path.Clean folds them and three or more folding to one.
func TestNormpathIsPosixpathNormpath(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "."},
		{".", "."},
		{"/", "/"},
		{"//", "//"},
		{"///", "/"},
		{"////a", "/a"},
		{"//a//b/../c", "//a/c"},
		{"//a/", "//a"},
		{"///a", "/a"},
		{"/..", "/"},
		{"//..", "//"},
		{"/../a", "/a"},
		{"../a", "../a"},
		{"a/./b/../../..", ".."},
		{"/a/b/", "/a/b"},
		{"/a/./b//c", "/a/b/c"},
	} {
		if got := Normpath(c.in); got != c.want {
			t.Errorf("Normpath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
