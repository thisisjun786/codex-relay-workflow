package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	t.Setenv("HOME", filepath.Join(root, "user-home"))
	passwd, err := user.Current()
	if err != nil {
		t.Fatal(err)
	}
	for i, override := range []string{"scopes", "./a/../scopes/", "~/scopes", "/" + filepath.Join(root, "s"), ""} {
		// Python's answer is asked here, in the test (recorded: see askPython); a root under the
		// passwd home is spelled so.
		t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", override)
		var want []string
		askPython(t, fmt.Sprintf("resolve_scope_root %d", i), &want, func() (any, error) {
			command := exec.Command(filepath.Join(repo, ".venv", "bin", "python"), "-c", `import json
from codex_session_relay.service import resolve_scope_root
root, authority = resolve_scope_root()
print(json.dumps([str(root), authority]))`)
			command.Dir = alias
			command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			raw, err := command.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("python: %v\n%s", err, raw)
			}
			var answer []string
			if err = json.Unmarshal(raw, &answer); err == nil && len(answer) == 2 && passwd.HomeDir != "" {
				answer[0] = strings.Replace(answer[0], passwd.HomeDir, "<passwd home>", 1)
			}
			return answer, err
		}, root, alias, override)
		if len(want) == 2 {
			want[0] = strings.Replace(want[0], "<passwd home>", passwd.HomeDir, 1)
		}
		t.Run("CODEX_SESSION_RELAY_SCOPE_DIR="+override, func(t *testing.T) {
			t.Setenv("CODEX_SESSION_RELAY_SCOPE_DIR", override)
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
