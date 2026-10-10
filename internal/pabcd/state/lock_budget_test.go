package state

import (
	"errors"
	"io/fs"
	"os"
	"sync"
	"testing"
	"time"
)

// The lock's wait budget is the oracle's (LOCK_RETRY_DELAYS_MS, about 250 ms in all) and giving up
// after it is deliberate: there is no stale-lock breaker. A test that races writers through
// withSessionLock therefore decides its outcome with disk speed as much as with the lock. These
// helpers let such a test widen the budget through the sleep seam, so what it measures is the
// property it means to measure.
const (
	// lockBudgetWiden multiplies every delay the lock schedules, so the same six writers are decided
	// by the lock rather than by how long one fsync takes on the machine running the test.
	lockBudgetWiden = 50

	// lockBudgetScheduleTotal is the whole of the oracle's retry schedule, 5+10+15+20+25+30+35+40+35+35 ms.
	lockBudgetScheduleTotal = 250 * time.Millisecond

	// lockBudgetReleaseAfter is the virtual time a waiter must have spent sleeping before the holder
	// of TestLockWaitBudgetDecidesWhetherASlowHolderBlocksTheWriter lets go: above the oracle's whole
	// schedule, so only a widened one reaches it, and below the first widened delay past it.
	lockBudgetReleaseAfter = 300 * time.Millisecond
)

// lockBudgetSleep is the sleep seam with the budget widened by lockBudgetWiden.
func lockBudgetSleep() func(time.Duration) {
	return func(d time.Duration) { time.Sleep(lockBudgetWiden * d) }
}

// TestLockWaitBudgetDecidesWhetherASlowHolderBlocksTheWriter pins the wait budget from both sides
// without a timing guess. A holder owns the lock and lets go only once the virtual time the waiter
// has spent sleeping passes lockBudgetReleaseAfter; the injected sleep advances that clock by
// d*widen and the handoff runs over channels, so nothing here waits on a wall clock.
//
// At the oracle's scale the virtual clock reaches only lockBudgetScheduleTotal, the schedule is
// exhausted, and withSessionLock returns the last create error without ever entering its critical
// section. At a widened scale it passes the release point, the holder lets go, and the waiter enters
// with a nil error.
func TestLockWaitBudgetDecidesWhetherASlowHolderBlocksTheWriter(t *testing.T) {
	for _, tc := range []struct {
		name    string
		widen   time.Duration
		wantErr bool
	}{
		{"the oracle's budget gives up", 1, true},
		{"a widened budget reaches the release", lockBudgetWiden, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			held, release, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			letGo := func() { once.Do(func() { close(release) }) }
			go func() { // the slow holder: in the lock until this test says otherwise
				defer close(released)
				_ = WithSessionLock(cwd, "counter", func() error {
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			lock := StatePath(cwd, "counter") + ".lock"
			t.Cleanup(letGo) // the give-up leg leaves the holder inside fn until it has asserted
			entered, slept := false, time.Duration(0)
			err := withSessionLock(cwd, "counter", func() error { entered = true; return nil }, func(d time.Duration) {
				slept += tc.widen * d
				if slept >= lockBudgetReleaseAfter {
					letGo()
					<-released // the holder is out and its lock removed before the next create
				}
			})
			if tc.wantErr {
				var pathErr *fs.PathError
				if !errors.As(err, &pathErr) || pathErr.Path != lock || !errors.Is(err, fs.ErrExist) || entered || slept != lockBudgetScheduleTotal {
					t.Fatalf("the oracle's budget: err %v entered %v slept %v", err, entered, slept)
				}
				// the holder still owns a lock nobody broke, which is the oracle's answer to a dead one
				if got := fileText(t, lock); got != sessionLockRecord(os.Getpid()) {
					t.Fatalf("the lock holds %q", got)
				}
				letGo()
				<-released
				return
			}
			if err != nil || !entered || slept < lockBudgetReleaseAfter {
				t.Fatalf("a widened budget: err %v entered %v slept %v", err, entered, slept)
			}
		})
	}
}

// WaitForSessionLock (CRW-1181) is the seam through which a test that cannot reach the sleep seam of withSessionLock (a hook, a
// command) makes the real WithSessionLock wait for its holder's release instead of giving up after the oracle's schedule. The
// test runs WithSessionLock itself against a holder that lets go only once the waiter's virtual clock (sessionLockSleep, which
// records the delay and does not sleep) passes the whole schedule, so it is decided by the wait the code schedules and not by a
// wall clock: without the seam the call gives up with the busy error, with it the call enters after the release, and removing the
// seam's connection to WithSessionLock fails the second leg.
func TestWaitForSessionLockOutlastsTheScheduleThroughWithSessionLock(t *testing.T) {
	for _, tc := range []struct {
		name    string
		patient bool
	}{
		{"the oracle's schedule gives up", false},
		{"a patient wait reaches the release", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cwd := t.TempDir()
			held, release, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			letGo := func() { once.Do(func() { close(release) }) }
			go func() {
				defer close(released)
				_ = WithSessionLock(cwd, "counter", func() error {
					close(held)
					<-release
					return nil
				})
			}()
			<-held
			t.Cleanup(letGo)
			slept := time.Duration(0)
			previous := sessionLockSleep
			sessionLockSleep = func(d time.Duration) {
				slept += d
				if slept > lockBudgetScheduleTotal {
					letGo()
					<-released // the holder is out and its lock removed before the next attempt
				}
			}
			t.Cleanup(func() { sessionLockSleep = previous })
			if tc.patient {
				defer WaitForSessionLock(time.Minute)()
			}
			entered := false
			err := WithSessionLock(cwd, "counter", func() error { entered = true; return nil })
			if !tc.patient {
				if !errors.Is(err, fs.ErrExist) || entered || slept != lockBudgetScheduleTotal {
					t.Fatalf("the oracle's schedule: err %v entered %v slept %v", err, entered, slept)
				}
				letGo()
				<-released
				return
			}
			if err != nil || !entered || slept <= lockBudgetScheduleTotal {
				t.Fatalf("a patient wait: err %v entered %v slept %v", err, entered, slept)
			}
		})
	}
	if got := sessionLockPatience.Load(); got != 0 {
		t.Fatalf("patience %d after the restore, want 0", got)
	}
}

// The patience ends at its guard: a holder that never lets go is answered with the busy error once the guard has passed.
func TestWaitForSessionLockGivesUpAtItsGuard(t *testing.T) {
	cwd := t.TempDir()
	held, release, released := make(chan struct{}), make(chan struct{}), make(chan struct{})
	go func() {
		defer close(released)
		_ = WithSessionLock(cwd, "counter", func() error { close(held); <-release; return nil })
	}()
	<-held
	t.Cleanup(func() { close(release); <-released })
	previous := sessionLockSleep
	var calls int
	sessionLockSleep = func(time.Duration) { calls++; time.Sleep(time.Millisecond) }
	t.Cleanup(func() { sessionLockSleep = previous })
	defer WaitForSessionLock(50 * time.Millisecond)()
	err := WithSessionLock(cwd, "counter", func() error { t.Error("entered a held lock"); return nil })
	if !errors.Is(err, fs.ErrExist) || calls <= lockBudgetScheduleLen {
		t.Fatalf("err %v after %d sleeps, want the busy error after more than the schedule's %d", err, calls, lockBudgetScheduleLen)
	}
}

// lockBudgetScheduleLen is the number of delays in the oracle's retry schedule.
const lockBudgetScheduleLen = 10
