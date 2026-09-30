package delivery

import (
	"encoding/json"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A store serving another socket lists, beside the socket-first line, the directory
// CODEX_SESSION_RELAY_STATE names, resolved as Python's _wrong_socket_recovery resolves it:
// Path(pinned).expanduser().resolve(). ~user is that user's passwd home, and a relative pin is
// read against the working directory the kernel names, not $PWD's spelling through a link. A pin
// under a directory this user may not search, or through a link loop, resolves as os.path.realpath
// keeps it and is offered. A ~user nothing answers is said rather than offered.
func TestAWrongSocketRecoveryResolvesThePinnedDirectoryAsPythonDoes(t *testing.T) {
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
	t.Chdir(alias)
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
		t.Run(pinned, func(t *testing.T) {
			t.Setenv(stateEnv, pinned)
			command := exec.Command(python, "-c", `import json, sys
from pathlib import Path
from codex_session_relay import cli
from codex_session_relay.store import StateSelection
cli.PROGRAM.set("PROG")
print(json.dumps(cli._wrong_socket_recovery(StateSelection(Path(sys.argv[1]), "flag", "d", None), "/r.sock", "/w.sock")))`, selected)
			command.Dir = alias
			command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			raw, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("python: %v\n%s", err, raw)
			}
			var want []string
			if err = json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			var got []string
			for _, line := range wrongSocketRecovery(store.StateSelection{Path: selected}, "/r.sock", "/w.sock") {
				got = append(got, strings.ReplaceAll(line.(string), program(), "PROG"))
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("go:\n%s\npython:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
