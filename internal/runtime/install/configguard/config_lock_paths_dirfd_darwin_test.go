//go:build darwin

package configguard

import (
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-993 d2: the search-only open flag opens a directory of mode 0300 and the by-name lookup works through it.
// It runs on macOS hardware or CI; it is compiled and vetted on Linux.
func TestConfigLockPathsDirFlagsOpenASearchOnlyDirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0300 directory")
	}
	dir := filepath.Join(t.TempDir(), "d")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "f"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	fd, err := unix.Open(dir, configLockPathsDirFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	var st unix.Stat_t
	if err := unix.Fstatat(fd, "f", &st, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
}
