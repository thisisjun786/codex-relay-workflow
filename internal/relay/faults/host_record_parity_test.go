package faults

import (
	"os"
	"os/user"
	"path/filepath"
	"testing"
)

// The sweep reads the host record where faultsweep.host_record_path named it: XDG_STATE_HOME when
// it is set, expanded and spelled as pathlib spells it, else $HOME/.local/state, where an unset
// HOME is "~" (the passwd home) and an empty one Path(""), the working directory. The daemon used
// to fail to start without a HOME, where Python answered. The golden holds Python's answers.
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
	// Where the host record is, for each environment, as the golden holds it: <root> and, for an
	// unset HOME, the passwd home stand for this run's.
	paths := runPaths{root, "<root>"}
	if account, err := user.Current(); err == nil && len(account.HomeDir) > 1 {
		paths = append(paths, account.HomeDir, "<passwd-home>")
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
			})
		}
	}()
	checkGolden(t, "host record paths", nil, paths, got)
}
