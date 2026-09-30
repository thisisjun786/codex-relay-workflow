package cli

import (
	"encoding/json"
	"os"
	"os/exec"
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
	repo := strings.TrimSuffix(mustGetwd(t), "/internal/relay/cli")
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
	for _, override := range []string{"scopes", "./a/../scopes/", "~/scopes", "/" + filepath.Join(root, "s"), ""} {
		t.Run("CODEX_SESSION_RELAY_SCOPE_DIR="+override, func(t *testing.T) {
			t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", override)
			command := exec.Command(filepath.Join(repo, ".venv", "bin", "python"), "-c", `import json
from codex_session_relay.service import resolve_scope_root
root, authority = resolve_scope_root()
print(json.dumps([str(root), authority]))`)
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
			if got, authority := scopeRoot(); got != want[0] || authority != want[1] {
				t.Errorf("%q %q, python %q", got, authority, want)
			}
		})
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}
