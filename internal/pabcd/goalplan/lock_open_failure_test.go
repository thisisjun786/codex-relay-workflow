//go:build unix

package goalplan

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"golang.org/x/sys/unix"
)

// CRW-1185 evaluation d1: a creation lock whose directory cannot be opened after the mkdir (a umask that strips the read
// bits leaves the lock directory 0333 for a non-root user) must not leave that directory behind, or every later attempt
// reports the slug busy for a lock nobody holds. The directory it created is removed; one it did not create stays.
func TestCreationLockOpenFailureLeavesNoLock(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a mode-0333 directory")
	}
	cwd := t.TempDir()
	old := unix.Umask(0o444)
	restored := false
	restore := func() {
		if !restored {
			unix.Umask(old)
			restored = true
		}
	}
	defer restore()
	t.Cleanup(func() {
		_ = filepath.WalkDir(cwd, func(p string, d fs.DirEntry, err error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
	})
	called := false
	res, err := WithGoalplanCreationLock(cwd, "fresh", func() error { called = true; return nil }, nil)
	restore()
	if err == nil || called || res.Kind == "ok" {
		t.Fatalf("an unopenable lock directory was taken: %+v %v called=%v", res, err, called)
	}
	status, serr := GoalplanWriteLockStatus(cwd, "fresh", nil)
	if serr != nil {
		_ = filepath.WalkDir(cwd, func(p string, d fs.DirEntry, e error) error {
			if d != nil && d.IsDir() {
				_ = os.Chmod(p, 0o700)
			}
			return nil
		})
		status, serr = GoalplanWriteLockStatus(cwd, "fresh", nil)
	}
	if serr != nil || status.Exists {
		t.Fatalf("the failed acquisition left its lock directory: %+v %v", status, serr)
	}
	if _, e := os.Lstat(status.Path); !errors.Is(e, os.ErrNotExist) {
		t.Fatalf("lock path after the failure: %v", e)
	}
	res, err = WithGoalplanCreationLock(cwd, "fresh", func() error { called = true; return nil }, nil)
	if err != nil || res.Kind != "ok" || !called {
		t.Fatalf("the next attempt under a normal umask: %+v %v called=%v", res, err, called)
	}
}

// The removal is by the identity recorded after the mkdir: a lock directory another holder put in its place stays.
func TestGoalplanLockRemoveCreatedKeepsAReplacement(t *testing.T) {
	root := t.TempDir()
	parent, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	if err := os.Mkdir(filepath.Join(root, GoalplanLockDir), 0o700); err != nil {
		t.Fatal(err)
	}
	var created unix.Stat_t
	if err := unix.Fstatat(int(parent.Fd()), GoalplanLockDir, &created, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "moved")
	if err := os.Rename(filepath.Join(root, GoalplanLockDir), moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(root, GoalplanLockDir), 0o700); err != nil {
		t.Fatal(err)
	}
	goalplanLockRemoveCreated(parent, created)
	if _, err := os.Lstat(filepath.Join(root, GoalplanLockDir)); err != nil {
		t.Fatalf("a replacement lock directory was removed: %v", err)
	}
	if err := os.Rename(moved, filepath.Join(root, "again")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, GoalplanLockDir)); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "again"), filepath.Join(root, GoalplanLockDir)); err != nil {
		t.Fatal(err)
	}
	goalplanLockRemoveCreated(parent, created)
	if _, err := os.Lstat(filepath.Join(root, GoalplanLockDir)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the directory this acquisition made stayed: %v", err)
	}
}
