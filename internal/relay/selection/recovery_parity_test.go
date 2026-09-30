package selection

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
)

// The refusal the CLI and the Stop hook share offers the directory CODEX_SESSION_RELAY_STATE
// names as _wrong_socket_recovery resolves it, Path(pinned).expanduser().resolve(): a pin under a
// directory this user may not search, or through a link loop, is kept as os.path.realpath keeps it
// and offered, and only a ~user nothing answers is said instead. Python's answer is recorded
// (pyoracle).
func TestAWrongSocketRecoveryResolvesThePinnedDirectoryAsPythonDoes(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	python := filepath.Join(strings.TrimSuffix(wd, "/internal/relay/selection"), ".venv", "bin", "python")
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
	// Python's answers are asked for before the chdir below: pyoracle finds a recording relative
	// to the package directory.
	answers := map[string][]byte{}
	for _, pin := range pins {
		answers[pin.name] = pyoracle.Answer(t, pin.name, func() ([]byte, error) {
			command := exec.Command(python, "-c", `import json, sys
from pathlib import Path
from codex_session_relay import cli
from codex_session_relay.store import StateSelection
cli.PROGRAM.set("PROG")
print(json.dumps(cli._wrong_socket_recovery(StateSelection(Path(sys.argv[1]), "flag", "d", None), "/r.sock", "/w.sock")))`, selected)
			command.Dir = root
			command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", store.StateEnv+"="+pin.pinned)
			raw, err := command.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("python: %v\n%s", err, raw)
			}
			return raw, nil
		}, pyoracle.Substitute(root, "<ROOT>"))
	}
	t.Chdir(root)
	for _, pin := range pins {
		raw := answers[pin.name]
		t.Run(pin.name, func(t *testing.T) {
			t.Setenv(store.StateEnv, pin.pinned)
			var want []string
			if err = json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			var got []string
			for _, line := range wrongSocketRecovery(Services{Selection: store.StateSelection{Path: selected}, Program: "PROG"}, "/r.sock", "/w.sock") {
				got = append(got, line.(string))
			}
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("go:\n%s\npython:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
			}
		})
	}
}
