//go:build dev

package ci

import (
	"strings"
	"testing"
)

// Todo 48's acceptance: this repository tracks no Python file.
func TestNoPythonIsTracked(t *testing.T) {
	out, err := runGit(repoRoot(), "ls-files", "-z")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range strings.Split(strings.TrimSuffix(string(out), "\x00"), "\x00") {
		if strings.HasSuffix(name, ".py") {
			t.Errorf("%s: a tracked Python file", name)
		}
	}
}
