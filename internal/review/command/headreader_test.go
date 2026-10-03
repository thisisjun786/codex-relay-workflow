package command

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestHeadReaderReadsCommittedObjectsOnly(t *testing.T) {
	r := newRepo(t)
	head := r.commit(map[string]string{"a.go": "one\ntwo\nthree", "empty.txt": "", "nl.txt": "\n", "dir/b.go": "x\n", ".gitattributes": "* diff=evil filter=evil\n"})
	// The working tree moves on, and the repository's own configuration would run a program for every diff, textconv and filter.
	marker := filepath.Join(t.TempDir(), "marker")
	for key, value := range map[string]string{"diff.external": "touch " + marker, "diff.evil.textconv": "touch " + marker, "filter.evil.clean": "touch " + marker, "filter.evil.smudge": "touch " + marker, "core.pager": "touch " + marker} {
		r.git("config", key, value)
	}
	if err := os.WriteFile(filepath.Join(r.dir, "a.go"), []byte("changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := &gitHead{ctx: context.Background(), repo: r.dir, head: head}
	for path, want := range map[string]int{"a.go": 3, "empty.txt": 0, "nl.txt": 1, "dir/b.go": 1} {
		if n, err := g.Lines(path); err != nil || n != want {
			t.Errorf("Lines(%q) = %d, %v, want %d", path, n, err, want)
		}
	}
	if got, err := g.ReadLines("a.go", 2, 3); err != nil || !slices.Equal(got, []string{"two", "three"}) {
		t.Errorf("ReadLines = %q, %v", got, err)
	}
	for _, c := range [][2]int{{0, 1}, {2, 1}, {1, 4}} {
		if _, err := g.ReadLines("a.go", c[0], c[1]); err == nil || errors.Is(err, fs.ErrNotExist) {
			t.Errorf("ReadLines(%d, %d) must fail with something other than not-exist: %v", c[0], c[1], err)
		}
	}
	for _, path := range []string{"missing.go", "dir", "", "a.go\x00"} {
		if _, err := g.Lines(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lines(%q): %v, want fs.ErrNotExist", path, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a configured program ran: %v", err)
	}
}
