//go:build dev

package ci

import (
	"strings"
	"testing"
)

// Todo 48's acceptance: this repository tracks no Python file and no script a python shebang
// runs. The check is the one `crw-dev ci validate` applies, over the tracked files only, so a
// scratch file breaks `crw-dev ci validate` but not `make test`.
func TestNoPythonIsTracked(t *testing.T) {
	root := repoRoot()
	out, err := runGit(root, "ls-files", "-z")
	if err != nil {
		t.Fatal(err)
	}
	names := strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00")
	for _, e := range pythonFileErrors(root, names) {
		t.Error(e)
	}
}
