package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

// CRW-871: WithSessionLockContext is WithSessionLock for a caller that can be interrupted (the
// orchestrate row under cmd/crw serve). A context cancelled before or during the wait creates no
// lock file, does not run fn and returns the context's own error; WithSessionLock keeps the
// oracle's behaviour and its sleep seam by calling this with context.Background().

// TestWithSessionLockContextCancelledCreatesNoLock pins the whole of the cancelled path: no lock
// file, fn not run, the context's error.
func TestWithSessionLockContextCancelledCreatesNoLock(t *testing.T) {
	cwd := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	ran := false
	err := WithSessionLockContext(ctx, cwd, "s", func() error { ran = true; return nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled lock returned %v, want context.Canceled", err)
	}
	if ran {
		t.Fatal("the cancelled lock ran fn")
	}
	if _, err := os.Lstat(StatePath(cwd, "s") + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the cancelled lock left a lock file: %v", err)
	}
}

// TestWithSessionLockContextCancelledAtTheGiveUpAnswersInterrupted is the CRW-922 (c1-3) case: a holder
// owns the lock, the waiter exhausts its retry budget, and the invocation's context is cancelled in the
// window just before the acquisition failure is returned. The call must answer the context's own error,
// not fs.ErrExist, because a cancelled invocation that never took the lock writes nothing and answers 130
// rather than reporting the busy error with code 1. The cancellation is fired from the give-up seam, and
// the case passes an empty retry schedule so it reaches the give-up on the first failed create: the case
// needs no sleep and does not depend on the oracle's real waits.
func TestWithSessionLockContextCancelledAtTheGiveUpAnswersInterrupted(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, lockOwnerLive()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(lockPath) }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := sessionLockBeforeGiveUp
	sessionLockBeforeGiveUp = func() { cancel() }
	defer func() { sessionLockBeforeGiveUp = previous }()

	ran := false
	err := orchestrateInterruptLockContext(ctx, cwd, "s", func() error { ran = true; return nil }, time.Sleep, []time.Duration{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("a lock that gave up under a cancelled invocation returned %v; want context.Canceled", err)
	}
	if ran {
		t.Fatal("the cancelled give-up ran fn")
	}
}

// TestWithSessionLockContextGiveUpWithALiveContextKeepsTheBusyError is the control: the same give-up
// under a live context still reports the acquisition error, so WithSessionLock's contract is unchanged. It
// passes the same empty schedule, so the control adds no real wait either.
func TestWithSessionLockContextGiveUpWithALiveContextKeepsTheBusyError(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, lockOwnerLive()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(lockPath) }()

	ran := false
	err := orchestrateInterruptLockContext(context.Background(), cwd, "s", func() error { ran = true; return nil }, time.Sleep, []time.Duration{})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("a live lock that gave up returned %v; want fs.ErrExist", err)
	}
	if ran {
		t.Fatal("the live give-up ran fn")
	}
}

// TestWithSessionLockContextCancelledDuringTheWait pins the wait: a holder owns the lock, the
// waiter is cancelled while it retries, and the waiter returns the context's error without
// entering fn or creating a second lock file.
func TestWithSessionLockContextCancelledDuringTheWait(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, lockOwnerLive()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(lockPath) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	ran := false
	go func() {
		done <- WithSessionLockContext(ctx, cwd, "s", func() error { ran = true; return nil })
	}()
	time.Sleep(20 * time.Millisecond) // the waiter is inside its first retry delay (5 ms..250 ms schedule)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("lock cancelled during the wait returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock wait did not end after its context was cancelled")
	}
	if ran {
		t.Fatal("the cancelled wait ran fn")
	}
}

// TestWithSessionLockContextLiveRunsAndReleases is the control: a live context takes the lock,
// runs fn and removes the lock file afterwards, exactly as WithSessionLock does.
func TestWithSessionLockContextLiveRunsAndReleases(t *testing.T) {
	cwd := t.TempDir()
	ran := false
	err := WithSessionLockContext(context.Background(), cwd, "s", func() error {
		if _, err := os.Lstat(StatePath(cwd, "s") + ".lock"); err != nil {
			t.Fatalf("the lock file is missing inside fn: %v", err)
		}
		ran = true
		return nil
	})
	if err != nil {
		t.Fatalf("live lock returned %v", err)
	}
	if !ran {
		t.Fatal("the live lock did not run fn")
	}
	if _, err := os.Lstat(StatePath(cwd, "s") + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the live lock left its lock file behind: %v", err)
	}
}

// TestWithSessionLockStillUsesItsSleepSeam keeps the oracle-facing entry unchanged: it delegates
// to the context form with a background context, so a caller's sleep seam is still what the wait
// uses and a busy lock still gives up with the last create error.
func TestWithSessionLockStillUsesItsSleepSeam(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "counter") + ".lock"
	if err := createExclusive(lockPath, lockOwnerLive()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(lockPath) }()
	slept := 0
	err := withSessionLock(cwd, "counter", func() error { return nil }, func(time.Duration) { slept++ })
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("busy lock returned %v, want fs.ErrExist", err)
	}
	if slept == 0 {
		t.Fatal("the busy lock did not use the caller's sleep seam")
	}
}
