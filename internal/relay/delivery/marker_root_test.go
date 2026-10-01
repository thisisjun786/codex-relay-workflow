package delivery

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The marker root is the directory Python's resolve_marker_root names (the golden began as its
// answers): Path.home() (an empty HOME is the root), exactly two leading slashes kept as pathlib
// keeps them, and a relative flag read against the working directory the kernel names rather than
// $PWD's spelling through a link.
func TestAMarkerRootIsTheDirectoryPythonNames(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err = os.MkdirAll(filepath.Join(root, "real", "wd"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "alias")); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias", "wd")
	// The call runs in alias, $PWD spelling it through the link, inside inDirectory: the goldens are
	// filed relative to this package's directory.
	t.Setenv("PWD", alias)
	t.Setenv(MarkerEnv, "")
	for _, c := range []struct{ name, home, xdg, flag string }{
		{"an empty HOME", "", "", ""},
		{"a HOME with two leading slashes", "/" + filepath.Join(root, "home"), "", ""},
		{"an XDG_STATE_HOME with two leading slashes", filepath.Join(root, "home"), "/" + filepath.Join(root, "xdg"), ""},
		{"a flag with two leading slashes", filepath.Join(root, "home"), "", "/" + filepath.Join(root, "m")},
		{"a relative flag", filepath.Join(root, "home"), "", "m"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("HOME", c.home)
			t.Setenv("XDG_STATE_HOME", c.xdg)
			var got MarkerSelection
			var err error
			inDirectory(t, alias, func() { got, err = ResolveMarkerRoot(c.flag) })
			golden.CheckJSON(t, "resolve_marker_root", []string{got.Path, got.Source, got.Detail}, golden.Substitute(filepath.Join(root, "home"), "<root-home>"), golden.Substitute(root, "<root>"))
			if err != nil {
				t.Fatalf("%+v: %v", got, err)
			}
		})
	}
}
