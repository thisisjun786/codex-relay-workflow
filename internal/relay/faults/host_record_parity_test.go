package faults

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

// The sweep reads the host record where faultsweep.host_record_path names it: XDG_STATE_HOME when
// it is set, expanded and spelled as pathlib spells it, else $HOME/.local/state, where an unset
// HOME is "~" (the passwd home) and an empty one Path(""), the working directory. The daemon used
// to fail to start without a HOME, where Python answers.
func TestTheHostRecordIsWherePythonLooksForIt(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	unset := "(unset)"
	cases := []struct{ name, xdg, home string }{
		{"an XDG_STATE_HOME", filepath.Join(root, "xdg"), filepath.Join(root, "user-home")},
		{"an XDG_STATE_HOME of two leading slashes and a dot", "/" + filepath.Join(root, ".", "xdg") + "/", filepath.Join(root, "user-home")},
		{"an XDG_STATE_HOME under ~", "~/xdg", filepath.Join(root, "user-home")},
		{"a relative XDG_STATE_HOME", "xdg", filepath.Join(root, "user-home")},
		{"HOME alone", "", filepath.Join(root, "user-home") + "/"},
		{"HOME of two leading slashes", "", "/" + filepath.Join(root, "user-home")},
		{"an empty HOME", "", ""},
		{"HOME unset", "", unset},
	}
	// Where Python looks, asked from root in each environment. Its answers are read before the
	// test moves to root: the recordings live beside this package.
	paths := pyPaths{root, "<root>"}
	if account, err := user.Current(); err == nil && len(account.HomeDir) > 1 {
		// Python expands an unset HOME to the passwd home.
		paths = append(paths, account.HomeDir, "<passwd-home>")
	}
	want := map[string]string{}
	for _, c := range cases {
		var path string
		pyValue(t, "host_record_path "+c.name, []string{c.xdg, c.home}, paths, &path, func() (any, error) {
			env := []string{"PYTHONDONTWRITEBYTECODE=1", "XDG_STATE_HOME=" + c.xdg}
			if c.home != unset {
				env = append(env, "HOME="+c.home)
			}
			for _, entry := range os.Environ() {
				if !strings.HasPrefix(entry, "HOME=") && !strings.HasPrefix(entry, "XDG_STATE_HOME=") {
					env = append(env, entry)
				}
			}
			command := exec.Command(filepath.Join(f1Root(), ".venv", "bin", "python"), "-c", `import json
from codex_session_relay.faultsweep import host_record_path
print(json.dumps(str(host_record_path())))`)
			command.Dir = root
			command.Env = env
			raw, err := command.CombinedOutput()
			if err != nil {
				return nil, fmt.Errorf("python: %v\n%s", err, raw)
			}
			var want string
			if err = json.Unmarshal(raw, &want); err != nil {
				return nil, fmt.Errorf("%v: %s", err, raw)
			}
			return want, nil
		})
		want[c.name] = path
	}
	// Go answers from root; the goldens are read and written from the package directory.
	got := map[string]string{}
	func() {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatal(err)
		}
		if err = os.Chdir(root); err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := os.Chdir(wd); err != nil {
				t.Fatal(err)
			}
		}()
		for _, c := range cases {
			t.Run(c.name, func(t *testing.T) {
				t.Setenv("XDG_STATE_HOME", c.xdg)
				t.Setenv("HOME", c.home)
				if c.home == unset {
					if err := os.Unsetenv("HOME"); err != nil {
						t.Fatal(err)
					}
				}
				path, err := HostRecordPath()
				if err != nil {
					t.Fatal(err)
				}
				got[c.name] = path
				if path != want[c.name] {
					t.Errorf("%q, python %q: %v", path, want[c.name], err)
				}
			})
		}
	}()
	checkGolden(t, "host record paths", nil, runPaths(paths), got)
}
