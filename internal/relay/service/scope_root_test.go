//go:build linux

package service

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/pyoracle"
	"golang.org/x/sys/unix"
)

// Audit finding 22: an isolated registry root is spelled as Python spells it
// (expanduser, physical cwd, pathlib's lexical form with '..' kept), so K.lock and the
// isolated scope key are the same file and key in both runtimes. Python is the oracle; its
// answers are recorded (pyoracle), asked before the chdir below because a recording is found
// relative to the package directory.
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
	socket := "/var/tmp/crw-scope-root/app.sock"
	overrides := []string{"a/../b", "//x", "///x/./y/", "/x/./y/", "~", "~/", "~/a/../s", "~" + current.Username + "/s", "rel/./scopes/", "."}
	// Each answer names its root and the key salted with it: the root is recorded with the
	// temporary directory, $HOME (the suite's) and this user's home as placeholders, and the salt as <SALT>, which a
	// replay derives again from the root it compares.
	answers := map[string][2]string{}
	for _, override := range overrides {
		key := strings.Replace(override, current.Username, "<USER>", 1)
		raw := pyoracle.Answer(t, key, func() ([]byte, error) {
			cmd := exec.Command(testPython[:len(testPython)-len("codex-session-relay")]+"python", "-c", "import json,sys\nfrom codex_session_relay import ownership, service\nroot, _ = service.resolve_scope_root()\nprint(json.dumps([str(root), ownership.scope_key(sys.argv[1])]))", socket)
			// Python's getcwd is physical; a $PWD naming the link must not change the spelling.
			cmd.Dir = link
			cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1", "PWD="+link, ScopeEnv+"="+override)
			raw, err := cmd.Output()
			if err != nil {
				return nil, fmt.Errorf("%v: %s", err, raw)
			}
			var answer [2]string
			if err = json.Unmarshal(raw, &answer); err != nil {
				return nil, fmt.Errorf("%v: %s", err, raw)
			}
			answer[1] = strings.Replace(answer[1], "isolated-"+scopeSalt(answer[0])+"-", "isolated-<SALT>-", 1)
			return marshalText(answer)
		}, pyoracle.Substitute(cwd, "<CWD>"), pyoracle.Substitute(os.Getenv("HOME"), "<HOME>"), pyoracle.Substitute(current.HomeDir, "<USERHOME>"))
		var want [2]string
		if err = json.Unmarshal(raw, &want); err != nil {
			t.Fatal(err, string(raw))
		}
		want[1] = strings.Replace(want[1], "<SALT>", scopeSalt(want[0]), 1)
		answers[override] = want
	}
	t.Chdir(link)
	t.Setenv("PWD", link)
	for _, override := range overrides {
		t.Run(override, func(t *testing.T) {
			t.Setenv(ScopeEnv, override)
			want := answers[override]
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
	// cleaned. K.lock and K.json are the files the kernel resolves for that spelling, and no
	// directory appears at the cleaned spelling. (That they are the very files a live Python
	// claim holds, which then excludes Go's, was checked here until todo 44: rollback to
	// Python closed at todo 43 (rollback_allowed=0), and the Python runtime leaves in todo 44.)
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
		// The daemon canonicalizes its socket, so this one's directory exists.
		socket := base + "/app.sock"
		scope, err := ResolveScope()
		if err != nil {
			t.Fatal(err)
		}
		lock := scope.path(socket, ".lock")
		if lock != base+"/link/../scopes/"+scope.Key(socket)+".lock" {
			t.Fatalf("K.lock %q is not the spelling kept lexically", lock)
		}
		// The built daemon claims the scope, serves one tick and releases it.
		daemon := exec.Command(testBinary, "relay", "--state", base+"/state", "--socket", socket, "daemon", "--max-ticks", "1", "--allow-isolated-scope")
		daemon.Env = os.Environ()
		if raw, err := daemon.Output(); err != nil {
			t.Fatalf("Go daemon: %v %s", err, raw)
		}
		resolved := filepath.Join(base, "deep", "scopes", scope.Key(socket))
		var st, kernel unix.Stat_t
		if err = unix.Stat(lock, &st); err != nil || unix.Stat(resolved+".lock", &kernel) != nil || st.Ino != kernel.Ino {
			t.Fatalf("Go K.lock %q is not %s.lock: %v", lock, resolved, err)
		}
		if get(scope.Read(socket), "storeId") == nil || get(read(resolved+".json"), "storeId") != get(scope.Read(socket), "storeId") {
			t.Fatalf("the registration is not at the kernel's spelling: %v", scope.Read(socket))
		}
		if _, err = os.Stat(filepath.Join(base, "scopes")); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("a registry appeared at the cleaned spelling: %v", err)
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

// scopeSalt is the salt an isolated scope key derives from its registry root.
func scopeSalt(root string) string {
	sum := sha256.Sum256([]byte(root))
	return hex.EncodeToString(sum[:4])
}
