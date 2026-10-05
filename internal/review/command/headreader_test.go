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
	if _, err := g.ReadLines("a.go", 2, 4); err == nil || errors.Is(err, fs.ErrNotExist) {
		t.Errorf("ReadLines beyond the file must fail with something other than not-exist: %v", err)
	}
	for _, path := range []string{"missing.go", "dir", "", "a.go\x00", ":/a.go"} {
		if _, err := g.Lines(path); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("Lines(%q): %v, want fs.ErrNotExist", path, err)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Errorf("a configured program ran: %v", err)
	}
}

// The paths of a finding are relative to the repository root whichever directory --repo names, so a root file is found from a subdirectory and a file of the same name below it is never read in its place.
func TestHeadReaderFromASubdirectoryReadsPathsFromTheRoot(t *testing.T) {
	r := newRepo(t)
	head := r.commit(map[string]string{"a.go": "1\n2\n3\n", "b.go": "1\n2\n", "sub/a.go": "x\n"})
	g := &gitHead{ctx: context.Background(), repo: filepath.Join(r.dir, "sub"), head: head}
	for path, want := range map[string]int{"a.go": 3, "b.go": 2, "sub/a.go": 1} {
		if n, err := g.Lines(path); err != nil || n != want {
			t.Errorf("Lines(%q) = %d, %v, want %d", path, n, err, want)
		}
	}
	if _, err := g.Lines("sub/b.go"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("Lines(sub/b.go): %v, want fs.ErrNotExist", err)
	}
}
