package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strconv"
	"time"
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
	if err := makeSessionsDir(cwd); err != nil {
		return err
	}
	lockPath := StatePath(cwd, sessionID) + ".lock"
	delays := [...]time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35} // milliseconds: LOCK_RETRY_DELAYS_MS
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := createExclusive(lockPath, strconv.Itoa(os.Getpid()))
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
	defer func() { _ = removeFile(lockPath) }()
	return fn()
}
