package faults

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
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
	t.Chdir(root)
	unset := "(unset)"
	for _, c := range []struct{ name, xdg, home string }{
		{"an XDG_STATE_HOME", filepath.Join(root, "xdg"), filepath.Join(root, "home")},
		{"an XDG_STATE_HOME of two leading slashes and a dot", "/" + filepath.Join(root, ".", "xdg") + "/", filepath.Join(root, "home")},
		{"an XDG_STATE_HOME under ~", "~/xdg", filepath.Join(root, "home")},
		{"a relative XDG_STATE_HOME", "xdg", filepath.Join(root, "home")},
		{"HOME alone", "", filepath.Join(root, "home") + "/"},
		{"HOME of two leading slashes", "", "/" + filepath.Join(root, "home")},
		{"an empty HOME", "", ""},
		{"HOME unset", "", unset},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("XDG_STATE_HOME", c.xdg)
			t.Setenv("HOME", c.home)
			if c.home == unset {
				if err := os.Unsetenv("HOME"); err != nil {
					t.Fatal(err)
				}
			}
			command := exec.Command(filepath.Join(f1Root(), ".venv", "bin", "python"), "-c", `import json
from codex_session_relay.faultsweep import host_record_path
print(json.dumps(str(host_record_path())))`)
			command.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
			raw, err := command.CombinedOutput()
			if err != nil {
				t.Fatalf("python: %v\n%s", err, raw)
			}
			var want string
			if err = json.Unmarshal(raw, &want); err != nil {
				t.Fatalf("%v: %s", err, raw)
			}
			if got, err := HostRecordPath(); err != nil || got != want {
				t.Errorf("%q, python %q: %v", got, want, err)
			}
		})
	}
}
