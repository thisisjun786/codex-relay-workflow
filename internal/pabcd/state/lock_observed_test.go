package state

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

// CRW-564: WithSessionLockObserved reports the first busy attempt and takes the caller's retry budget; with neither it is
// WithSessionLock.

// TestWithSessionLockObservedReportsTheWaitOnceAndRunsAfterTheRelease releases the holder from inside onBusy, so the order is the
// lock's own: busy, release, acquire, fn.
func TestWithSessionLockObservedReportsTheWaitOnceAndRunsAfterTheRelease(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, "1"); err != nil {
		t.Fatal(err)
	}
	busy, ran := 0, false
	err := WithSessionLockObserved(cwd, "s", func() error { ran = true; return nil }, []time.Duration{5, 5, 5}, func() {
		busy++
		if ran {
			t.Error("fn ran before the lock was released")
		}
		_ = removeFile(lockPath)
	})
	if err != nil || !ran || busy != 1 {
		t.Fatalf("err=%v ran=%v busy=%d; want nil, true, 1", err, ran, busy)
	}
}

// TestWithSessionLockObservedGivesUpOnTheCallersBudget pins that the budget is the caller's: an empty schedule gives up on the
// first busy attempt with the busy error and never runs fn.
func TestWithSessionLockObservedGivesUpOnTheCallersBudget(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lockPath := StatePath(cwd, "s") + ".lock"
	if err := createExclusive(lockPath, "1"); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = removeFile(lockPath) }()
	ran := false
	err := WithSessionLockObserved(cwd, "s", func() error { ran = true; return nil }, []time.Duration{}, nil)
	if !errors.Is(err, fs.ErrExist) || ran {
		t.Fatalf("err=%v ran=%v; want fs.ErrExist and fn not run", err, ran)
	}
}
