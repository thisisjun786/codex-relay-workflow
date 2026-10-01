package store

import (
	"encoding/json"
	"os"
	"os/user"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// The package's expected values are goldens (internal/testsupport/golden, testdata/golden), and
// the stores the retired Python implementation wrote, which Go reads back, are fixtures
// (testdata/fixtures). golden reads and writes both relative to the working directory, which a
// test here may have changed, so every read and check goes through inPackageDir.

// packageDir is this package's directory: the test binary starts in it.
var packageDir = func() string { wd, _ := os.Getwd(); return wd }()

// inPackageDir runs f from the package directory and returns to the test's working directory
// after it. A golden is written by a cleanup when updating; the two cleanups registered around f
// put the package directory back for it and the test's afterwards.
func inPackageDir(t testing.TB, f func()) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if wd == packageDir {
		f()
		return
	}
	t.Cleanup(func() { _ = os.Chdir(wd) })
	if err = os.Chdir(packageDir); err != nil {
		t.Fatal(err)
	}
	f()
	t.Cleanup(func() { _ = os.Chdir(packageDir) })
	if err = os.Chdir(wd); err != nil {
		t.Fatal(err)
	}
}

// checkJSON is golden.CheckJSON from the package directory.
func checkJSON(t testing.TB, key string, got any, opts ...golden.Option) {
	t.Helper()
	inPackageDir(t, func() { golden.CheckJSON(t, key, got, opts...) })
}

// checkText is golden.Check of a text from the package directory.
func checkText(t testing.TB, key, got string, opts ...golden.Option) {
	t.Helper()
	inPackageDir(t, func() { golden.Check(t, key, []byte(got), opts...) })
}

// wantText is golden.Want of a text from the package directory, for a golden the test reads as
// an input to what it then checks: produce, Go's own answer, runs only when updating.
func wantText(t testing.TB, key string, produce func() string, opts ...golden.Option) string {
	t.Helper()
	var out []byte
	inPackageDir(t, func() { out = golden.Want(t, key, func() []byte { return []byte(produce()) }, opts...) })
	return string(out)
}

// readFixture decodes the named fixture (testdata/fixtures/<name>, or <name>.gz) into out.
func readFixture(t testing.TB, name string, out any) {
	t.Helper()
	var raw []byte
	inPackageDir(t, func() { raw = golden.Fixture(t, name) })
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("fixture %s: %v", name, err)
	}
}

// passwdHomeSubstitution names this account's passwd home, which Path.home() answers with HOME
// unset, <PASSWD-HOME> in a golden.
func passwdHomeSubstitution() golden.Option {
	if u, err := user.Current(); err == nil && u.HomeDir != "/" {
		return golden.Substitute(u.HomeDir, "<PASSWD-HOME>")
	}
	return golden.Substitute("", "")
}

// defaultScopeSubstitution names the directory discovery scopes by the default App Server socket
// (DefaultSocket, under the test's CODEX_HOME or HOME) <DEFAULT-SCOPE> in a golden: the name is a
// digest of a path below the test's temporary directories.
func defaultScopeSubstitution(t testing.TB) golden.Option {
	t.Helper()
	socket, err := DefaultSocket()
	if err != nil {
		t.Fatal(err)
	}
	scope, err := SocketScope(socket)
	if err != nil {
		t.Fatal(err)
	}
	return golden.Substitute(scope, "<DEFAULT-SCOPE>")
}
