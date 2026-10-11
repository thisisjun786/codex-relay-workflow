package integrator

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrLockHeld is the refusal of a lock another process holds.
var ErrLockHeld = errors.New("another integration run holds the lock")

// acquireLock takes the integration lock without waiting: an exclusive flock, the lock the
// shell runner takes with flock(1) on the same file. The file is created when missing and
// never removed (another process may hold it), and a link planted at its path is refused.
func acquireLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o664)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, fmt.Errorf("%w: %s", ErrLockHeld, path)
		}
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}

// lockState reports whether the lock is free without creating or changing anything: a
// shared non-blocking flock on the existing file, released at once.
func lockState(path string) string {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "absent"
		}
		return "unreadable"
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return "held"
		}
		return "unreadable"
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	return "free"
}
