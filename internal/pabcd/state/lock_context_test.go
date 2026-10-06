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

// TestWithSessionLockContextCancelledDuringTheWait pins the wait: a holder owns the lock, the
// waiter is cancelled while it retries, and the waiter returns the context's error without
// entering fn or creating a second lock file.
func TestWithSessionLockContextCancelledDuringTheWait(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, "999999"); err != nil {
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
	if err := createExclusive(lockPath, "999999"); err != nil {
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
