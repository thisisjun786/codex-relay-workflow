package cli

import (
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
)

// The scope root the worker policy check compares with a record is Python's resolve_scope_root:
// Path(override).expanduser().absolute(), a relative override read against the working directory
// the kernel names rather than $PWD's spelling through a link, and the passwd home's production
// root without one. The owner records the same root, so a check reading another spelling would
// refuse a matching record as worker_policy_service_mismatch.
func TestTheScopeRootIsTheOnePythonResolves(t *testing.T) {
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
	t.Setenv("HOME", filepath.Join(root, "user-home"))
	passwd, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for i, override := range []string{"scopes", "./a/../scopes/", "~/scopes", "/" + filepath.Join(root, "s"), ""} {
		// The golden began as Python's resolve_scope_root; a root under the passwd home is
		// spelled so.
		t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", override)
		var compared []string
		t.Run("CODEX_SESSION_RELAY_SCOPE_DIR="+override, func(t *testing.T) {
			t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", override)
			got, authority := scopeRoot()
			// A root under the passwd home is spelled so.
			if passwd.HomeDir != "" {
				got = strings.Replace(got, passwd.HomeDir, "<passwd home>", 1)
			}
			compared = []string{got, authority}
		})
		if compared != nil {
			expectGolden(t, fmt.Sprintf("resolve_scope_root %d", i), compared, root, alias, override)
		}
	}
}
