package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"time"

	"golang.org/x/sys/unix"
)

// WithSessionLock runs fn holding the session's exclusive lock, the file <state file>.lock that holds the pid (withSessionLock).
// There is deliberately no stale-lock breaker: reading, renaming and unlinking by path is racy, and two processes that both
// judge a lock stale can enter together and lose a verdict. Acquisition instead gives up after about 250 ms and returns the
// error of the last create (errors.Is(err, fs.ErrExist)), so a lock left by a dead process costs a denied completion, visible
// and recoverable, and a stale .lock file is removed by hand. The lock is released by path, errors ignored, also when fn panics.
func WithSessionLock(cwd, sessionID string, fn func() error) error {
	return WithSessionLockContext(context.Background(), cwd, sessionID, fn)
}

func withSessionLock(cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	return orchestrateInterruptLockContext(context.Background(), cwd, sessionID, fn, sleep)
}

// WithSessionLockContext is WithSessionLock for a caller that can be interrupted (the orchestrate row under cmd/crw
// serve, whose invocation context the first SIGINT cancels). The retry schedule is the same; a context cancelled
// before or during the wait creates no lock file, does not run fn and returns the context's own error. A caller with
// no context passes context.Background(), which is what WithSessionLock does: its behaviour and its sleep seam are
// unchanged.
func WithSessionLockContext(ctx context.Context, cwd, sessionID string, fn func() error) error {
	return orchestrateInterruptLockContext(ctx, cwd, sessionID, fn, time.Sleep)
}

func orchestrateInterruptLockContext(ctx context.Context, cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// The sessions directory is opened through a descriptor walk that refuses a symbolic link at any
	// step (openSessionsDir), and the lock file is created, read and removed RELATIVE TO THAT
	// DESCRIPTOR. That is what makes the creation symlink-safe: a pathname create would follow a .crw
	// link swapped in after any earlier observation and land the lock file outside the workspace, and a
	// pathname remove would follow it again. The held descriptor pins the directory inode for the whole
	// critical section, so neither this call nor a concurrent rename of a path component can redirect
	// the write or the unlink (CRW-646, failure class 3).
	dir, err := openSessionsDir(cwd)
	if err != nil {
		return err
	}
	defer dir.Close()
	lockName := SanitizeKey(sessionID) + ".json.lock"
	lockPath := StatePath(cwd, sessionID) + ".lock"
	delays := [...]time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35} // milliseconds: LOCK_RETRY_DELAYS_MS
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := createExclusiveAt(dir, lockName, lockPath, strconv.Itoa(os.Getpid()))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= len(delays) {
			return err
		}
		delay := delays[attempt] * time.Millisecond
		if ctx.Done() == nil {
			sleep(delay) // a context that can never end keeps the caller's seam
			continue
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer func() { _ = unix.Unlinkat(int(dir.Fd()), lockName, 0) }()
	return fn()
}

// createExclusiveAt is createExclusive for a name under an already-open directory: writeFileSync with
// flag "wx", created relative to dir's descriptor with O_NOFOLLOW so a symbolic link standing at the
// lock name is refused rather than followed. EEXIST is the busy answer the retry schedule handles; the
// file this call creates holds the pid and is closed before the caller's critical section runs.
func createExclusiveAt(dir *os.File, name, displayPath, data string) error {
	fd, err := unix.Openat(int(dir.Fd()), name, unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0o666)
	if err != nil {
		return &os.PathError{Op: "open", Path: displayPath, Err: err}
	}
	f := os.NewFile(uintptr(fd), displayPath)
	_, err = f.WriteString(data)
	return errors.Join(err, f.Close())
}
