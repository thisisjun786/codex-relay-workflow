package state

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// TestMakeSessionsDirNeedsOnlyTraversalOnTheStateRoot is CRW-646 d1: the state root is opened with the
// platform's search-only flag (O_PATH on Linux), so a .crw the caller may traverse but not read still
// works. dev's pathname creates needed only traversal of the root, and an O_RDONLY walk regressed that
// for every EnsureState, WriteState and WithSessionLock caller. Darwin has no O_PATH, so a search-only
// root fails closed there by design and the case does not apply.
func TestMakeSessionsDirNeedsOnlyTraversalOnTheStateRoot(t *testing.T) {
	if runtime.GOOS == "darwin" {
		t.Skip("Darwin has no O_PATH: a search-only state root fails closed there by design")
	}
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(cwd, crwdir.DirName)
	if err := os.Chmod(root, 0o111); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(root, 0o777) }) // so t.TempDir can remove it

	if err := makeSessionsDir(cwd); err != nil {
		t.Fatalf("makeSessionsDir on a search-only state root: %v", err)
	}
	if err := WithSessionLock(cwd, "s", func() error { return nil }); err != nil {
		t.Fatalf("WithSessionLock on a search-only state root: %v", err)
	}
	if err := WriteState(cwd, DefaultState("s", "")); err != nil {
		t.Fatalf("WriteState on a search-only state root: %v", err)
	}
}

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

// TestWithSessionLockReleasesALockTakenInADisplacedDirectory is CRW-646 d1: after a lock file is
// created, the directory it was created in is checked against the path the walk resolved, and a lock
// taken in a directory a rename has since displaced is released and retried. Without that check the
// waiter would hold a lock on an inode the current path no longer reaches, and two writers could each
// hold "the" lock on different inodes (the loser of the rename could then rewrite state the winner's
// lock protected).
//
// The seam displaces the directory between the create and the check, and the fresh directory carries a
// second holder's lock, so the retry meets it and fails with the lock's own EEXIST: the callback must
// never run, and the displaced directory must be left without the lock this attempt created.
func TestWithSessionLockReleasesALockTakenInADisplacedDirectory(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	sessions := filepath.Join(cwd, crwdir.DirName, SessionsSubdir)
	lockName := SanitizeKey("s") + ".json.lock"
	displaced := sessions + ".old"
	sessionLockAfterCreate = func() {
		sessionLockAfterCreate = nil // once
		if err := os.Rename(sessions, displaced); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(sessions, 0o777); err != nil {
			t.Error(err)
			return
		}
		// A second holder owns the fresh directory's lock, so the retry cannot take it.
		if err := os.WriteFile(filepath.Join(sessions, lockName), []byte("999999"), 0o666); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { sessionLockAfterCreate = nil })

	ran := false
	err := withSessionLock(cwd, "s", func() error { ran = true; return nil }, func(time.Duration) {})
	if !errors.Is(err, fs.ErrExist) {
		t.Fatalf("the lock returned %v, want fs.ErrExist from the fresh directory's holder", err)
	}
	if ran {
		t.Fatal("the callback ran while holding a lock in the displaced directory")
	}
	if _, err := os.Lstat(filepath.Join(displaced, lockName)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("the displaced directory kept the lock this attempt created: %v", err)
	}
}
