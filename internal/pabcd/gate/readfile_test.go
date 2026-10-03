package gate

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// readFile is the one read of a receipt or manifest artifact: it follows no link at the last element, which a path checked a
// moment earlier may have become, and does not block on a pipe that took the file's place.
func TestReadFileReadsOnlyAnUnlinkedRegularFile(t *testing.T) {
	dir := t.TempDir()
	file, link, pipe := filepath.Join(dir, "file"), filepath.Join(dir, "link"), filepath.Join(dir, "pipe")
	writeFile(t, file, "data")
	must(t, os.Symlink(file, link))
	must(t, syscall.Mkfifo(pipe, 0o644))
	if got, err := readFile(file, 4); err != nil || string(got) != "data" {
		t.Fatalf("a regular file read as %q, %v", got, err)
	}
	if got, err := readFile(file, 3); err == nil {
		t.Errorf("a file over the limit read as %q", got)
	}
	for _, path := range []string{link, pipe} {
		if got, err := readFile(path, 4); err == nil {
			t.Errorf("%s read as %q", filepath.Base(path), got)
		}
	}
}
