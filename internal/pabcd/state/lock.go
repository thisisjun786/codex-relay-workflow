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

// sessionLockBeforeGiveUp is a test seam: it runs inside orchestrateInterruptLockContext immediately
// before an acquisition failure is returned, so a test can cancel the invocation's context in that
// window without a sleep. Production leaves it nil.
var sessionLockBeforeGiveUp func()

func withSessionLock(cwd, sessionID string, fn func() error, sleep func(time.Duration)) error {
	return orchestrateInterruptLockContext(context.Background(), cwd, sessionID, fn, sleep, nil)
}

// WithSessionLockContext is WithSessionLock for a caller that can be interrupted (the orchestrate row under cmd/crw
// serve, whose invocation context the first SIGINT cancels). The retry schedule is the same; a context cancelled
// before or during the wait creates no lock file, does not run fn and returns the context's own error. A caller with
// no context passes context.Background(), which is what WithSessionLock does: its behaviour and its sleep seam are
// unchanged.
func WithSessionLockContext(ctx context.Context, cwd, sessionID string, fn func() error) error {
	retryDelays, onBusy := lockWaitProbe()
	return orchestrateInterruptLockWait(ctx, cwd, sessionID, fn, time.Sleep, retryDelays, onBusy)
}

// orchestrateInterruptLockContext is the acquisition both entries share. retryDelays is a test seam: a
// caller that has to reach the give-up passes a short schedule so the case does not burn the oracle's
// real waits (CRW-922); nil means LOCK_RETRY_DELAYS_MS, which is what both entries above pass.
func orchestrateInterruptLockContext(ctx context.Context, cwd, sessionID string, fn func() error, sleep func(time.Duration), retryDelays []time.Duration) error {
	return orchestrateInterruptLockWait(ctx, cwd, sessionID, fn, sleep, retryDelays, nil)
}

// WithSessionLockObserved is WithSessionLock for a test that has to know the caller is waiting. onBusy runs once, on the first
// attempt that finds the lock held, immediately before the caller sleeps for the first time; retryDelays replaces the oracle's
// schedule (milliseconds, as LOCK_RETRY_DELAYS_MS) so the test, not the wall clock, decides when the wait gives up. A nil onBusy
// and a nil retryDelays are exactly WithSessionLock. Production never passes either (CRW-564).
func WithSessionLockObserved(cwd, sessionID string, fn func() error, retryDelays []time.Duration, onBusy func()) error {
	return orchestrateInterruptLockWait(context.Background(), cwd, sessionID, fn, time.Sleep, retryDelays, onBusy)
}

func orchestrateInterruptLockWait(ctx context.Context, cwd, sessionID string, fn func() error, sleep func(time.Duration), retryDelays []time.Duration, onBusy func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := makeSessionsDir(cwd); err != nil {
		return err
	}
	lockPath := StatePath(cwd, sessionID) + ".lock"
	delays := [...]time.Duration{5, 10, 15, 20, 25, 30, 35, 40, 35, 35} // milliseconds: LOCK_RETRY_DELAYS_MS
	schedule := delays[:]
	if retryDelays != nil {
		schedule = retryDelays
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := createExclusive(lockPath, strconv.Itoa(os.Getpid()))
		if err == nil {
			break
		}
		if !errors.Is(err, fs.ErrExist) || attempt >= len(schedule) {
			if sessionLockBeforeGiveUp != nil {
				sessionLockBeforeGiveUp()
			}
			// CRW-922 (c1-3): the give-up is reported after the invocation's context is read once more, so
			// a cancellation that landed as the wait ended answers the context's own error (130) rather
			// than the busy error with code 1 - the same rule the D close's goalplan locks follow. A
			// caller with no context (context.Background, which WithSessionLock passes) is unchanged: its
			// ctx.Err() is always nil.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			return err
		}
		if attempt == 0 && onBusy != nil {
			onBusy()
		}
		delay := schedule[attempt] * time.Millisecond
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
