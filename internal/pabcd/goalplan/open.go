package goalplan

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// These descriptors pin lookup against link swaps. Cooperating writers must not
// remove active locks or relocate their directories; no pathname API makes that atomic.
func openPlanDir(cwd, slug string) (*os.File, string, error) {
	if _, err := GoalplanDir(cwd, slug); err != nil {
		return nil, "", err
	}
	base, err := filepath.Abs(cwd)
	if err == nil {
		base, err = filepath.EvalSymlinks(base)
	}
	if err != nil {
		return nil, "", err
	}
	dir := filepath.Join(base, ".crw", GoalplansSubdir, slug)
	fd, err := unix.Open("/", directoryOpenFlags()|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, dir, err
	}
	f := os.NewFile(uintptr(fd), "/")
	expected := "/"
	for _, part := range strings.Split(strings.TrimPrefix(dir, "/"), "/") {
		expected = filepath.Join(expected, part)
		next, e := openAt(f, part, expected, directoryOpenFlags(), true, 0)
		_ = f.Close()
		if e != nil {
			return nil, dir, e
		}
		f = next
	}
	return f, dir, nil
}

func boundFile(f *os.File, expected string, directory bool) error {
	info, err := f.Stat() // fstat, never a second pathname lookup
	if err != nil {
		return err
	}
	if directory && !info.IsDir() || !directory && !info.Mode().IsRegular() {
		return fmt.Errorf("goalplan path is not a %s: %s", map[bool]string{true: "directory", false: "regular file"}[directory], expected)
	}
	actual, err := descriptorPath(f)
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("goalplan descriptor path %q is not %q", actual, expected)
	}
	return nil
}

func openAt(parent *os.File, name, expected string, flags int, directory bool, mode uint32) (*os.File, error) {
	fd, err := unix.Openat(int(parent.Fd()), name, flags|unix.O_NOFOLLOW|unix.O_CLOEXEC|unix.O_NONBLOCK, mode)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: expected, Err: err}
	}
	f := os.NewFile(uintptr(fd), expected)
	if err := boundFile(f, expected, directory); err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}
func pathAbsent(err error) bool { return errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESTALE) }

// Cleanup never follows a link. A replacement observed before cleanup is retained;
// the final inode check and unlink are not atomic against a noncooperating remover.
func releaseLock(parent, lock *os.File, dir string) {
	defer lock.Close()
	if boundFile(parent, filepath.Dir(dir), true) != nil || boundFile(lock, dir, true) != nil {
		return
	}
	own, e := lock.Stat()
	if e != nil {
		return
	}
	current, e := openAt(parent, GoalplanLockDir, dir, unix.O_RDONLY|unix.O_DIRECTORY, true, 0)
	if e != nil {
		return
	}
	info, e := current.Stat()
	_ = current.Close()
	if e != nil || !os.SameFile(own, info) {
		return
	}
	if removeContents(lock, dir) == nil {
		_ = unix.Unlinkat(int(parent.Fd()), GoalplanLockDir, unix.AT_REMOVEDIR)
	}
}
func removeContents(dir *os.File, path string) error {
	entries, err := dir.ReadDir(-1)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			child, e := openAt(dir, name, filepath.Join(path, name), unix.O_RDONLY|unix.O_DIRECTORY, true, 0)
			if e != nil {
				return e
			}
			e = removeContents(child, filepath.Join(path, name))
			_ = child.Close()
			if e != nil {
				return e
			}
			err = unix.Unlinkat(int(dir.Fd()), name, unix.AT_REMOVEDIR)
		} else {
			err = unix.Unlinkat(int(dir.Fd()), name, 0)
		}
		if err != nil {
			return err
		}
	}
	return nil
}
