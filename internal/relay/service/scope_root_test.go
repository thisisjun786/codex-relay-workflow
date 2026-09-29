//go:build linux

package service

import (
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"golang.org/x/sys/unix"
)

// Audit finding 22: an isolated registry root is spelled as Python spells it
// (expanduser, physical cwd, pathlib's lexical form with '..' kept), so K.lock and the
// isolated scope key are the same file and key in both runtimes. Python is the oracle.
func Test30IsolatedScopeRootMatchesPython(t *testing.T) {
	current, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	cwd := t.TempDir()
	real := filepath.Join(cwd, "real")
	if err = os.Mkdir(real, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(cwd, "link")
	if err = os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// Python's getcwd is physical; a $PWD naming the link must not change the spelling.
	t.Chdir(link)
	t.Setenv("PWD", link)
	socket := "/var/tmp/crw-scope-root/app.sock"
	for _, override := range []string{"a/../b", "//x", "///x/./y/", "/x/./y/", "~", "~/", "~/a/../s", "~" + current.Username + "/s", "rel/./scopes/", "."} {
		t.Run(override, func(t *testing.T) {
			t.Setenv(ScopeEnv, override)
			cmd := exec.Command(testPython[:len(testPython)-len("codex-session-relay")]+"python", "-c", "import json,sys\nfrom codex_session_relay import ownership, service\nroot, _ = service.resolve_scope_root()\nprint(json.dumps([str(root), ownership.scope_key(sys.argv[1])]))", socket)
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			raw, err := cmd.Output()
			if err != nil {
				t.Fatal(err, string(raw))
			}
			var want [2]string
			if err = json.Unmarshal(raw, &want); err != nil {
				t.Fatal(err, string(raw))
			}
			scope, err := ResolveScope()
			if err != nil {
				t.Fatal(err)
			}
			key, err := ownership.ScopeKey(socket)
			if err != nil {
				t.Fatal(err)
			}
			if scope.Root != want[0] || key != want[1] || scope.Key(socket) != want[1] {
				t.Fatalf("Go root=%q key=%q registry=%q; Python root=%q key=%q", scope.Root, key, scope.Key(socket), want[0], want[1])
			}
		})
	}
	// Review of finding 22: a '..' after a symlink is resolved by the kernel, never
	// cleaned. K.lock and K.json are the very files Python names (compared by inode),
	// so a live Python claim excludes Go's, and no directory appears at the cleaned
	// spelling.
	t.Run("symlink-then-dotdot", func(t *testing.T) {
		base := t.TempDir()
		inner := filepath.Join(base, "deep", "inner")
		if err := os.MkdirAll(inner, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(inner, filepath.Join(base, "link")); err != nil {
			t.Fatal(err)
		}
		t.Setenv(ScopeEnv, base+"/link/../scopes")
		python := exec.Command(testPython[:len(testPython)-len("codex-session-relay")]+"python", "-c", "import json,os,sys\nfrom codex_session_relay import service\nroot, authority = service.resolve_scope_root()\nregistry = service.ScopeRegistry(root, authority)\nclaim = registry.claim(sys.argv[1], {'storeId': 'python-store'})\nlock = registry.root / f'{registry.key(sys.argv[1])}.lock'\nprint(json.dumps([claim['ok'], str(lock), os.stat(lock).st_ino]), flush=True)\nsys.stdin.read()", socket)
		python.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
		python.Stderr = os.Stderr
		hold, err := python.StdinPipe()
		if err != nil {
			t.Fatal(err)
		}
		answer, err := python.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		if err = python.Start(); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = hold.Close(); _ = python.Wait() }()
		var claimed []any
		if err = json.NewDecoder(answer).Decode(&claimed); err != nil || len(claimed) != 3 || claimed[0] != true {
			t.Fatalf("python claim: %v %v", claimed, err)
		}
		pythonLock, _ := claimed[1].(string)
		pythonInode, _ := claimed[2].(float64)
		scope, err := ResolveScope()
		if err != nil {
			t.Fatal(err)
		}
		lock := scope.path(socket, ".lock")
		var st unix.Stat_t
		if err = unix.Stat(lock, &st); err != nil || st.Ino != uint64(pythonInode) || lock != pythonLock {
			t.Fatalf("Go K.lock %q (inode %d, %v); Python %q (inode %.0f)", lock, st.Ino, err, pythonLock, pythonInode)
		}
		if f, err := lockIfFree(lock); f != nil || err != nil {
			if f != nil {
				_ = f.Close()
			}
			t.Fatalf("Go took the scope lock a live Python claim holds: %v", err)
		}
		// The built daemon claims the same scope and is refused, instead of serving.
		daemon := exec.Command(testBinary, "relay", "--state", base+"/state", "--socket", socket, "daemon", "--max-ticks", "1", "--allow-isolated-scope")
		daemon.Env = os.Environ()
		raw, err := daemon.Output()
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 2 || !strings.Contains(string(raw), `"reason": "scope_owned_by_other_store"`) {
			t.Fatalf("Go daemon beside a live Python claim: %v %s", err, raw)
		}
		if get(scope.Read(socket), "storeId") != "python-store" || len(scope.Conflicts(socket, "go-store", "/elsewhere")) != 1 {
			t.Fatalf("Go does not read Python's registration: %v", scope.Read(socket))
		}
		if _, err = os.Stat(filepath.Join(base, "scopes")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a registry appeared at the cleaned spelling: %v", err)
		}
		if err = hold.Close(); err != nil {
			t.Fatal(err)
		}
		if err = python.Wait(); err != nil {
			t.Fatal(err)
		}
		f, err := lockIfFree(lock)
		if f == nil || err != nil {
			t.Fatalf("released scope lock still refused: %v", err)
		}
		_ = f.Close()
	})
	// An unknown ~user has no home in either runtime (Python raises RuntimeError).
	t.Setenv(ScopeEnv, "~crw-no-such-user-30/scopes")
	if scope, err := ResolveScope(); err == nil {
		t.Fatalf("unknown ~user resolved to %q", scope.Root)
	}
	if _, err := ownership.ScopeKey(socket); err == nil {
		t.Fatal("unknown ~user produced a scope key")
	}
}
