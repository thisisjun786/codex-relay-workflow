package cli_test

import (
	"os"
	"path/filepath"
	"testing"
)

// A read-only command names the store it did not find as Python's resolve_state_dir spells it:
// two leading slashes kept, and a relative --state read against the working directory the
// kernel names, not the $PWD spelling that reached it through a symbolic link.
func TestAnAbsentStoreIsNamedAsPythonSpellsItsPath(t *testing.T) {
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
	t.Chdir(filepath.Join(root, "alias", "wd"))
	argvs := [][]string{
		{"--state", "/" + filepath.Join(root, "st"), "--json", "store-identity"},
		{"--state", "st", "--json", "store-identity"},
	}
	want := pythonCLI(t, argvs...)
	for i, argv := range argvs {
		if got := goCLI(t, argv...); got != want[i] {
			t.Errorf("%q:\ngo     %d %s\npython %d %s", argv, got.code, got.stdout, want[i].code, want[i].stdout)
		}
	}
	for _, name := range []string{"st", "real/wd/st", "alias/wd/st"} {
		if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
			t.Errorf("a read-only command created %s: %v", name, err)
		}
	}
}
