//go:build linux

package service

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

// relayWrite runs one writable relay command on the state directory with Go's real entry point
// and returns its exit code and stdout.
func relayWrite(t *testing.T, home string) (int, string) {
	t.Helper()
	cmd := exec.Command(testBinary, "relay", "--state", home+"/state", "store-challenge", "--write")
	cmd.Env = environment(home)
	raw, err := cmd.Output()
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		t.Fatal(err)
	}
	if exit != nil {
		return exit.ExitCode(), string(raw)
	}
	return 0, string(raw)
}

func stateTree(t *testing.T, state string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(state)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(state, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = info.Mode().String() + " " + string(raw)
	}
	return out
}

// Review of decision D3: an owner-only lock is trusted in any directory, as Python and
// every earlier Go build trust it; only a lock that grants group or other access needs
// an owner-only directory. A group-writable state directory (0775 under umask 002) is
// created and served by Go, a store the retained Python fence owns there reaches the
// ownership decision instead of a lock refusal, and Go never leaves behind a lock file
// it then refuses (which D0 would refuse as a partial store forever).
func Test30GroupWritableStateDirectoryLocks(t *testing.T) {
	defer unix.Umask(unix.Umask(0o002))
	home := t.TempDir()
	goHome := home
	state := home + "/state"
	if err := os.Mkdir(state, 0o775); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if code, out := relayWrite(t, home); code != 0 {
			t.Fatalf("Go in a 0775 state directory: %d %s", code, out)
		}
	}
	for _, name := range []string{"relay.sqlite3", "takeover.json", "write-gate.lock"} {
		info, err := os.Stat(filepath.Join(state, name))
		if err != nil {
			t.Fatalf("incomplete store: %v", err)
		}
		if name == "write-gate.lock" && info.Mode().Perm() != 0o600 {
			t.Fatalf("created gate mode %o", info.Mode().Perm())
		}
	}

	home = t.TempDir()
	state = home + "/state"
	if err := os.Mkdir(state, 0o775); err != nil {
		t.Fatal(err)
	}
	// A store the Python fence owns there, as its absent-store initializer leaves it (testsupport
	// stamps it; the fence's own write here ran until todo 44, when the Python runtime left).
	func() {
		defer inRuntimeScope(t, home)()
		testsupport.Create(t, state+"/relay.sqlite3", "", "python")
	}()
	// The gate is trusted, so Go reaches the ownership decision (Python owns the store).
	if code, out := relayWrite(t, home); code != 2 || !strings.Contains(out, "the relay store belongs to another runtime") {
		t.Fatalf("Go on the Python store in a 0775 state directory: %d %s", code, out)
	}
	// On the Python store the lock-free check_start refuses first, as the fence's does (decision
	// 31), so a gate granting group access is never opened there and nothing changes.
	if err := os.Chmod(state+"/write-gate.lock", 0o664); err != nil {
		t.Fatal(err)
	}
	before := stateTree(t, state)
	if code, out := relayWrite(t, home); code != 2 || !strings.Contains(out, "the relay store belongs to another runtime") {
		t.Fatalf("0664 gate of the Python store in a 0775 state directory: %d %s", code, out)
	}
	if !reflect.DeepEqual(before, stateTree(t, state)) {
		t.Fatal("refused write changed the state directory")
	}
	// On Go's own store a gate granting group access in a group-writable directory is refused
	// unchanged; the same gate in an owner-only directory is trusted (the live host's 0664 locks).
	state = goHome + "/state"
	if err := os.Chmod(state+"/write-gate.lock", 0o664); err != nil {
		t.Fatal(err)
	}
	before = stateTree(t, state)
	if code, out := relayWrite(t, goHome); code != 2 || !strings.Contains(out, "unsafe lock file") {
		t.Fatalf("0664 gate in a 0775 state directory: %d %s", code, out)
	}
	if !reflect.DeepEqual(before, stateTree(t, state)) {
		t.Fatal("refused gate changed the state directory")
	}
	if err := os.Chmod(state, 0o700); err != nil {
		t.Fatal(err)
	}
	if code, out := relayWrite(t, goHome); code != 0 {
		t.Fatalf("0664 gate in an owner-only state directory: %d %s", code, out)
	}
}
