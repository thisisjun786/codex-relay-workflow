package selection

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The refusal the CLI and the Stop hook share offers the directory CODEX_SESSION_RELAY_STATE
// names as _wrong_socket_recovery resolves it, Path(pinned).expanduser().resolve(): a pin under a
// directory this user may not search, or through a link loop, is kept as os.path.realpath keeps it
// and offered, and only a ~user nothing answers is said instead. The goldens began as the lines
// Python's _wrong_socket_recovery answered for each pin.
func TestAWrongSocketRecoveryResolvesThePinnedDirectoryAsPythonDoes(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	locked := filepath.Join(root, "locked")
	if err = os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	for link, target := range map[string]string{"loopa": "loopb", "loopb": "loopa"} {
		if err = os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	selected := filepath.Join(root, "selected")
	pins := []struct{ name, pinned string }{
		{"locked-inner", filepath.Join(locked, "inner", "st")},
		{"loop-absolute", filepath.Join(root, "loopa", "st")},
		{"loop-relative", "loopa"},
		{"relative", "st"},
		{"selected", selected},
		{"unknown-user", "~crw-no-such-user-here/st"},
	}
	for _, pin := range pins {
		t.Run(pin.name, func(t *testing.T) {
			t.Setenv(store.StateEnv, pin.pinned)
			// The recovery is read from root, where the relative pins lie; the golden is read and
			// written from the package directory.
			if err := os.Chdir(root); err != nil {
				t.Fatal(err)
			}
			lines := wrongSocketRecovery(Services{Selection: store.StateSelection{Path: selected}, Program: "PROG"}, "/r.sock", "/w.sock")
			if err := os.Chdir(wd); err != nil {
				t.Fatal(err)
			}
			got := []string{}
			for _, line := range lines {
				got = append(got, line.(string))
			}
			golden.CheckJSON(t, "recovery", got, golden.Substitute(root, "<ROOT>"))
		})
	}
}
