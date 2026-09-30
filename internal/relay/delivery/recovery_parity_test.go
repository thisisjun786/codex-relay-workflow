package delivery

import (
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// A store serving another socket lists, beside the socket-first line, the directory
// CODEX_SESSION_RELAY_STATE names, resolved as Python's _wrong_socket_recovery resolves it:
// Path(pinned).expanduser().resolve(). ~user is that user's passwd home, and a relative pin is
// read against the working directory the kernel names, not $PWD's spelling through a link. A pin
// under a directory this user may not search, or through a link loop, resolves as os.path.realpath
// keeps it and is offered. A ~user nothing answers is said rather than offered. The lines are
// checked against the golden, which began as what _wrong_socket_recovery answered.
func TestAWrongSocketRecoveryResolvesThePinnedDirectoryAsPythonDoes(t *testing.T) {
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
	t.Setenv("HOME", filepath.Join(root, "home"))
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	selected := filepath.Join(root, "selected")
	locked := filepath.Join(root, "locked")
	if err = os.MkdirAll(filepath.Join(locked, "inner"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o700) })
	for link, target := range map[string]string{"loopa": "loopb", "loopb": "loopa"} {
		if err = os.Symlink(target, filepath.Join(alias, link)); err != nil {
			t.Fatal(err)
		}
	}
	// The ~user pin names a directory that does not exist under that home; resolving it reads no
	// more than the home's own entry.
	for _, pinned := range []string{"st", "~" + current.Username + "/crw-test-absent-" + filepath.Base(root), "~crw-no-such-user-here/st", "~/st",
		filepath.Join(locked, "inner", "st"), filepath.Join(alias, "loopa", "st"), "loopa"} {
		// Named without this run's directory or this user's name: the golden is found by name.
		label := strings.NewReplacer(root, "<root>", current.Username, "<user>").Replace(pinned)
		t.Run(label, func(t *testing.T) {
			t.Setenv(stateEnv, pinned)
			var got []string
			inDirectory(t, alias, func() {
				for _, line := range wrongSocketRecovery(store.StateSelection{Path: selected}, "/r.sock", "/w.sock") {
					got = append(got, strings.ReplaceAll(line.(string), program(), "PROG"))
				}
			})
			golden.Check(t, "wrong_socket_recovery", []byte(strings.Join(got, "\n")), golden.Substitute(filepath.Join(root, "home"), "<root-home>"), golden.Substitute(root, "<root>"), golden.Substitute(current.HomeDir, "<user-home>"), golden.Substitute("~"+current.Username, "~<user>"))
		})
	}
}
