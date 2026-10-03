package agy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/sys/unix"
)

// lockPoll is how often a call that is waiting for the host-wide lock tries it again.
const lockPoll = 50 * time.Millisecond

var errLockWait = errors.New("agy: the host-wide lock was not free in time")

// DefaultLockPath is the host-wide lock file: crw/review/agy.lock under XDG_STATE_HOME, or under ~/.local/state.
func DefaultLockPath() string {
	state := os.Getenv("XDG_STATE_HOME")
	if state == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = os.TempDir()
		}
		state = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(state, "crw", "review", "agy.lock")
}

// acquire takes an exclusive flock on path, trying every lockPoll until wait has passed (a negative wait tries once). It returns errLockWait when the lock
// stayed busy and the context's error when the caller gave up. The descriptor is close-on-exec, so agy never inherits the lock, and the file is never removed.
func acquire(ctx context.Context, path string, wait time.Duration) (release func(), err error) {
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the agy lock: %w", err)
	}
	deadline := time.Now().Add(wait)
	for {
		if err = ctx.Err(); err != nil {
			_ = unix.Close(fd)
			return nil, err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return func() { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			_ = unix.Close(fd)
			return nil, fmt.Errorf("lock %s: %w", path, err)
		}
		if !time.Now().Before(deadline) {
			_ = unix.Close(fd)
			return nil, errLockWait
		}
		select {
		case <-ctx.Done():
			_ = unix.Close(fd)
			return nil, ctx.Err()
		case <-time.After(lockPoll):
		}
	}
}
