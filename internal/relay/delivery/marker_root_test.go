package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The marker root is the directory Python's resolve_marker_root names (as recorded): Path.home() (an empty HOME
// is the root), exactly two leading slashes kept as pathlib keeps them, and a relative flag read
// against the working directory the kernel names rather than $PWD's spelling through a link.
func TestAMarkerRootIsTheDirectoryPythonNames(t *testing.T) {
	python := filepath.Join(repoRoot(t), ".venv", "bin", "python")
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
	// Each side runs in alias, $PWD spelling it through the link: Python's command there, and the Go
	// call inside inDirectory (the recordings are filed relative to this package's directory).
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
			raw := pyAnswer(t, "resolve_marker_root", func() ([]byte, error) {
				command := exec.Command(python, "-c", `import json, sys
from codex_session_relay.marker import resolve_marker_root
selected = resolve_marker_root(sys.argv[1] or None)
print(json.dumps([str(selected.path), selected.source, selected.detail]))`, c.flag)
				command.Dir = alias
				command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
				return pythonOutput(command)
			}, pyoracle.Substitute(filepath.Join(root, "home"), "<root-home>"), pyoracle.Substitute(root, "<root>"))
			var want []string
			if err := json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err)
			}
			var got MarkerSelection
			var err error
			inDirectory(t, alias, func() { got, err = ResolveMarkerRoot(c.flag) })
			if err != nil || got.Path != want[0] || got.Source != want[1] || got.Detail != want[2] {
				t.Errorf("%+v: %v\npython %q", got, err, want)
			}
		})
	}
}
