//go:build darwin

package goalplan

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-581 (CRW-357 Darwin): the goalplan directory open is search-only, so a directory of mode 0300 (write and search,
// no read) still opens and takes a new entry by name. The O_RDONLY open this replaces failed with EACCES. It runs on macOS
// hardware or CI; it is compiled and vetted on Linux with GOOS=darwin, and it is not run on macOS (recorded as unverified).
func TestGoalplanDirectoryOpenFlagsOpenASearchOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0300 directory")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	fd, err := unix.Open(dir, directoryOpenFlags()|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatalf("search-only open of a mode-0300 directory: %v", err)
	}
	defer unix.Close(fd)
	created, err := unix.Openat(fd, "entry", unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		t.Fatalf("create an entry by name through the search-only descriptor: %v", err)
	}
	_ = unix.Close(created)
}
