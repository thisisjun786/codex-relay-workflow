package tools

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// createRootCRW980Umask sets the process umask for one test and restores it after. The umask is process
// state, so a test that uses it must not run in parallel.
func createRootCRW980Umask(t *testing.T, mask int) {
	t.Helper()
	saved := syscall.Umask(mask)
	t.Cleanup(func() { syscall.Umask(saved) })
}

// C5: a directory made while the umask denies its owner reading is recorded and removed. The directory
// cannot be opened, so the record is kept by identity read through the parent.
func TestCreateRootCRW980RecordsADirectoryTheModeDoesNotLetItRead(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "downloads")
	createRootCRW980Umask(t, 0o400)

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot with a umask that denies reading the new directory: %v", err)
	}
	if _, found := review836RecordFor(created, target); !found {
		t.Fatalf("createRoot recorded %v, want %s", created, target)
	}
	removeCreated(created)
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s survived the cleanup: %v", target, err)
	}
}

// C5: a recorded directory whose mode is changed to write and search only is still removed through its
// parent, which needs nothing from the directory itself.
func TestCreateRootCRW980RemovesARecordedDirectoryWithoutReadPermission(t *testing.T) {
	base := t.TempDir()
	target := filepath.Join(base, "downloads")
	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot: %v", err)
	}
	if err := os.Chmod(target, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(target, 0o700) })

	removeCreated(created)
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s survived the cleanup after its mode changed to 0300: %v", target, err)
	}
}

// createRootIDOfInfo is the identity of a directory read with os.Lstat, for tests that hold one.
func createRootIDOfInfo(t *testing.T, info os.FileInfo) createRootID {
	t.Helper()
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatal("this host does not report the device and inode")
	}
	return createRootID{dev: uint64(st.Dev), ino: uint64(st.Ino)}
}
