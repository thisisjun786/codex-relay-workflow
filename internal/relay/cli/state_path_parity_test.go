package cli_test

import (
	"os"
	"path/filepath"
	"reflect"
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

// doctor under an XDG_STATE_HOME of two leading slashes names every directory as Python does:
// the selection, a store beside it that records no socket, and the transport ledger each keep
// both slashes, where filepath.Join and filepath.Dir would fold them to one.
func TestDoctorKeepsTwoLeadingSlashesInEveryDirectoryItNames(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "user-home"))
	t.Setenv("CODEX_HOME", filepath.Join(root, "codex"))
	t.Setenv("CODEX_SESSION_RELAY_STATE", "")
	t.Setenv("XDG_STATE_HOME", "/"+filepath.Join(root, "xdg"))
	beside := filepath.Join(root, "xdg", "codex-session-relay", "beside")
	if err = os.MkdirAll(beside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(beside, "relay.sqlite3"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	argv := []string{"--socket", "/x/s.sock", "--json", "doctor"}
	want := object(t, pythonCLI(t, argv)[0].stdout)
	got := object(t, goCLI(t, argv...).stdout)
	if sibling := want["siblingStores"].(map[string]any)["withoutProvenance"].([]any); len(sibling) != 1 || sibling[0] != "/"+beside {
		t.Fatalf("Python named %v", want["siblingStores"])
	}
	for _, key := range []string{"stateSelection", "ledger", "siblingStores"} {
		if !reflect.DeepEqual(got[key], want[key]) {
			t.Errorf("%s:\ngo     %v\npython %v", key, got[key], want[key])
		}
	}
}
