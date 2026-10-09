package evidence

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// fileLockWait bounds how long a writer of this package waits for one of its own record locks. The holders are hook processes
// that finish in milliseconds; a lock still held after this long is reported as an error and the caller takes its failure path.
const fileLockWait = 5 * time.Second

// errLockBusy is what withFileLock answers when the lock stayed held for fileLockWait.
var errLockBusy = errors.New("evidence record lock is busy")

// withFileLock runs fn holding an exclusive flock(2) on path, which is created (mode 0600) when it is missing; its directory must
// exist. The kernel drops the lock when the process ends, so a holder that dies leaves nothing to break by hand, unlike the session
// lock's pid file. The holder removes the file before it lets go, so the lock leaves nothing behind; a waiter that locked a file
// already removed (or replaced by a newer holder's) finds that the path no longer names what it holds and starts again, so two
// holders never run together. A link at path is refused (O_NOFOLLOW).
//
// Lock order (CRW-1106): a record lock of this package is taken first and the session lock (state.WithSessionLock) inside it,
// never the other way round, so a writer that records a terminal verdict while it holds a counter lock cannot deadlock against
// one that holds the session lock.
func withFileLock(path string, fn func() error) error {
	deadline := time.Now().Add(fileLockWait)
	for {
		f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
		if err != nil {
			return err
		}
		held, err := flockUntil(f, deadline)
		if err != nil {
			_ = f.Close()
			return err
		}
		if held {
			defer f.Close()
			defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
			defer func() { _ = os.Remove(path) }() // runs first: removed while still held
			return fn()
		}
		_ = f.Close()
		if time.Now().After(deadline) {
			return errLockBusy
		}
	}
}

// flockUntil takes the exclusive lock of f, retrying until deadline, and reports whether path still names the file it locked.
// false with no error means the previous holder removed it: the caller opens the path again.
func flockUntil(f *os.File, deadline time.Time) (bool, error) {
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			return false, err
		}
		if time.Now().After(deadline) {
			return false, errLockBusy
		}
		time.Sleep(2 * time.Millisecond)
	}
	held, err := f.Stat()
	if err != nil {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, err
	}
	named, err := os.Lstat(f.Name())
	if err != nil || !os.SameFile(held, named) {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		return false, nil
	}
	return true, nil
}

func stateDir(cwd string) string { return filepath.Join(cwd, crwdir.DirName) }

// ensureStateDir is crwdir.EnsureDir that also refuses a link or a file at cwd/.crw.
func ensureStateDir(cwd string) (string, error) {
	dir, err := crwdir.EnsureDir(cwd)
	if err == nil {
		err = requireDirectory(dir)
	}
	return dir, err
}

// ensureRecordDir creates cwd/.crw (with its .gitignore) and the directories below it named by parts, one by one, and refuses a
// link or anything that is not a directory at each of them, so no record of this package is written through a link planted in
// the state tree. It returns the deepest directory.
func ensureRecordDir(cwd string, parts ...string) (string, error) {
	dir, err := ensureStateDir(cwd)
	if err != nil {
		return "", err
	}
	for _, part := range parts {
		dir = filepath.Join(dir, part)
		if err := os.Mkdir(dir, 0o777); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
		if err := requireDirectory(dir); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// existingRecordDir is the directory below cwd/.crw named by parts when every step is a real directory, without creating
// anything. A missing step is fs.ErrNotExist; a link or a file at a step is an error of its own.
func existingRecordDir(cwd string, parts ...string) (string, error) {
	dir := stateDir(cwd)
	if err := requireDirectory(dir); err != nil {
		return "", err
	}
	for _, part := range parts {
		dir = filepath.Join(dir, part)
		if err := requireDirectory(dir); err != nil {
			return "", err
		}
	}
	return dir, nil
}
