package role

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// These tests pin the lock protocol of the dispatch ledger: taking a lock and giving it back hold a shared flock on the session
// directory, and a clear holds it exclusively from before its judgment to the removal of its tombstone, so nothing can take,
// release or replace a lock while a clear decides about it. HOME, CODEX_HOME and CRW_HOME point into temporary directories.

const (
	dispatchLockMutexSettle = 150 * time.Millisecond // how long a call that must be waiting is watched
	dispatchLockMutexGuard  = 10 * time.Second       // bounds every wait of a test and the shared wait of its directories
)

func dispatchLockMutexHomes(t *testing.T) {
	t.Helper()
	for _, name := range []string{"HOME", "CODEX_HOME", "CRW_HOME"} {
		t.Setenv(name, t.TempDir())
	}
}

// dispatchLockMutexOpen is one more process of the ledger: its own directory object, so its own open file descriptions.
func dispatchLockMutexOpen(t *testing.T, ws string, after func(string)) *dispatchPinnedDir {
	t.Helper()
	dir := must(dispatchDirectory(ws, lockClearSession, after))
	dir.dispatchLockMutexBound = dispatchLockMutexGuard
	t.Cleanup(func() { dir.Close() })
	return dir
}

// dispatchLockMutexSeam stops a clear at the named points of its directory object until the test lets it go. A point is
// reached while the clear holds its exclusive lock, so what a test starts after that can only wait for it.
type dispatchLockMutexSeam struct{ reached, gate map[string]chan struct{} }

func dispatchLockMutexStops(t *testing.T, points ...string) *dispatchLockMutexSeam {
	t.Helper()
	s := &dispatchLockMutexSeam{map[string]chan struct{}{}, map[string]chan struct{}{}}
	for _, point := range points {
		s.reached[point], s.gate[point] = make(chan struct{}), make(chan struct{})
	}
	t.Cleanup(func() {
		for _, point := range points {
			s.let(point)
		}
	})
	return s
}

// after is the callback of the clear's directory object.
func (s *dispatchLockMutexSeam) after(point string) {
	if reached, ok := s.reached[point]; ok {
		close(reached)
		<-s.gate[point]
	}
}

func (s *dispatchLockMutexSeam) stopped(t *testing.T, point string) {
	t.Helper()
	select {
	case <-s.reached[point]:
	case <-time.After(dispatchLockMutexGuard):
		t.Fatalf("the clear did not reach %q", point)
	}
}

// let lets the clear go on from point.
func (s *dispatchLockMutexSeam) let(point string) {
	select {
	case <-s.gate[point]:
	default:
		close(s.gate[point])
	}
}

// dispatchLockMutexCall is a call of the ledger running on its own goroutine.
type dispatchLockMutexCall struct {
	done chan struct{}
	err  error
}

func dispatchLockMutexRun(f func() error) *dispatchLockMutexCall {
	c := &dispatchLockMutexCall{done: make(chan struct{})}
	go func() { defer close(c.done); c.err = f() }()
	return c
}

// returns reports whether the call returned within wait.
func (c *dispatchLockMutexCall) returns(wait time.Duration) bool {
	select {
	case <-c.done:
		return true
	case <-time.After(wait):
		return false
	}
}

func (c *dispatchLockMutexCall) result(t *testing.T) error {
	t.Helper()
	if !c.returns(dispatchLockMutexGuard) {
		t.Fatal("a call of the ledger did not return")
	}
	return c.err
}

// dispatchLockMutexHold takes the record lock of name through dir and gives it an owner that has finished, which is what a
// clear is meant to remove.
func dispatchLockMutexHold(t *testing.T, dir *dispatchPinnedDir, name string) (release func() error, lock string) {
	t.Helper()
	release = must(dir.lock(name))
	lock = filepath.Join(dir.path, name+".lock")
	lockClearOwn(t, lock, lockClearDead(t))
	return release, lock
}

// While a clear holds the session directory between its judgment and the removal of the lock, a release and two acquisitions
// wait for it: the lock it judged is neither released nor replaced, and no two processes hold one lock. Without the shared
// lock, A releases, B takes the lock, the clear moves B's lock away and C takes the free name.
func TestDispatchLockMutexClearExcludesAcquireAndRelease(t *testing.T) {
	dispatchLockMutexHomes(t)
	ws := t.TempDir()
	const name = "task-one.json"
	release, lock := dispatchLockMutexHold(t, dispatchLockMutexOpen(t, ws, nil), name)
	identity, owner := must(os.Lstat(lock)), must(os.ReadFile(filepath.Join(lock, dispatchLockOwnerFile)))
	// The release after the clear is a call of a holder whose lock is already gone. An inode that the clear frees goes to the
	// next directory made on some filesystems, so the lock is kept open: its inode stays taken, and no later lock can look like it.
	pin := must(os.Open(lock))
	t.Cleanup(func() { pin.Close() })
	seam := dispatchLockMutexStops(t, "judged", "claimed")
	clearer := dispatchLockMutexOpen(t, ws, seam.after)
	cleared := dispatchLockMutexRun(func() error { _, err := dispatchLockClear(clearer, name+".lock", "test"); return err })
	seam.stopped(t, "judged")

	var holders atomic.Int32
	acquire := func(dir *dispatchPinnedDir) func() error {
		return func() error {
			_, err := dir.lock(name)
			if err == nil {
				holders.Add(1)
			}
			return err
		}
	}
	releasing := dispatchLockMutexRun(release)
	if releasing.returns(dispatchLockMutexSettle) {
		t.Errorf("the release returned while a clear held the session directory: %v", releasing.err)
	}
	taking := dispatchLockMutexRun(acquire(dispatchLockMutexOpen(t, ws, nil)))
	if taking.returns(dispatchLockMutexSettle) {
		t.Errorf("an acquisition returned while a clear held the session directory: %v", taking.err)
	}
	if now, err := os.Lstat(lock); err != nil || !os.SameFile(identity, now) || !bytes.Equal(must(os.ReadFile(filepath.Join(lock, dispatchLockOwnerFile))), owner) {
		t.Errorf("the lock the clear judged was changed while the clear held the directory: %v", err)
	}
	seam.let("judged")
	seam.stopped(t, "claimed")
	retaking := dispatchLockMutexRun(acquire(dispatchLockMutexOpen(t, ws, nil)))
	if retaking.returns(dispatchLockMutexSettle) {
		t.Errorf("an acquisition returned between the claim and the removal of the lock: %v", retaking.err)
	}
	seam.let("claimed")

	if err := cleared.result(t); err != nil {
		t.Errorf("the clear of the lock it judged: %v", err)
	}
	if err := releasing.result(t); err == nil {
		t.Error("the release of a lock that the clear had removed reported no error")
	}
	taking.result(t)
	retaking.result(t)
	if n := holders.Load(); n != 1 {
		t.Errorf("%d processes hold the lock, want exactly one", n)
	}
	if clearer.dispatchLockMutexHeld != nil {
		t.Error("the directory object still counts as holding the exclusive lock after its clear")
	}
	if tombs := must(filepath.Glob(lock + ".clearing-*")); len(tombs) != 0 {
		t.Errorf("a lock was left under a tombstone name: %v", tombs)
	}
	if held, _ := os.ReadFile(filepath.Join(lock, dispatchLockOwnerFile)); !strings.Contains(string(held), "\"pid\":"+strconv.Itoa(os.Getpid())+",") {
		t.Errorf("the lock at the name is not the one a live process took: %q", held)
	}
	if lines := strings.Count(string(must(os.ReadFile(filepath.Join(lockClearDir(ws), dispatchLockClearLog)))), "\n"); lines != 1 {
		t.Errorf("lock-clears.jsonl holds %d lines, want 1", lines)
	}
}

// A shared wait that is not granted in time answers like a held lock for an acquisition, and refuses a release, which then
// leaves the lock alone. Any name of the session is excluded, not only the one the clear works on.
func TestDispatchLockMutexBusyAnswersAfterTheBound(t *testing.T) {
	dispatchLockMutexHomes(t)
	ws := t.TempDir()
	held := dispatchLockMutexOpen(t, ws, nil)
	held.dispatchLockMutexBound = 100 * time.Millisecond
	release, lock := dispatchLockMutexHold(t, held, "task-one.json")
	identity := must(os.Lstat(lock))
	seam := dispatchLockMutexStops(t, "judged")
	cleared := dispatchLockMutexRun(func() error {
		_, err := dispatchLockClear(dispatchLockMutexOpen(t, ws, seam.after), "task-one.json.lock", "test")
		return err
	})
	seam.stopped(t, "judged")

	if err := release(); err == nil || !strings.Contains(err.Error(), "not released") {
		t.Errorf("release during a clear: %v", err)
	}
	if now, err := os.Lstat(lock); err != nil || !os.SameFile(identity, now) {
		t.Errorf("the refused release touched the lock: %v", err)
	}
	other := dispatchLockMutexOpen(t, ws, nil)
	other.dispatchLockMutexBound = 100 * time.Millisecond
	_, err := other.lock("task-two.json")
	if !errors.Is(err, fs.ErrExist) || err == nil || !strings.Contains(err.Error(), "task-two.json.lock: file exists") || lockClearExists(filepath.Join(other.path, "task-two.json.lock")) {
		t.Errorf("acquisition of a free name during a clear: %v", err)
	}
	seam.let("judged")
	check(t, cleared.result(t))
	if _, err := other.lock("task-two.json"); err != nil {
		t.Errorf("acquisition after the clear: %v", err)
	}
}

// Takers and releasers share the directory with each other: only a clear is excluded, so two of them never wait for one another.
func TestDispatchLockMutexSharedHoldersDoNotExcludeEachOther(t *testing.T) {
	dispatchLockMutexHomes(t)
	ws := t.TempDir()
	first, second := dispatchLockMutexOpen(t, ws, nil), dispatchLockMutexOpen(t, ws, nil)
	second.dispatchLockMutexBound = 100 * time.Millisecond
	dropFirst, err := first.dispatchLockMutexShared()
	check(t, err)
	defer dropFirst()
	dropSecond, err := second.dispatchLockMutexShared()
	if err != nil {
		t.Fatalf("a second shared holder of the directory waited for the first: %v", err)
	}
	dropSecond()
}

// A release removes the directory its holder created and nothing else: a lock that another process put at the name, a link,
// and a lock that is gone are all refused with an error and left alone.
func TestDispatchLockMutexReleaseRemovesOnlyItsOwnLock(t *testing.T) {
	for _, tc := range []struct {
		name string
		swap func(t *testing.T, session, lock string)
		kept []string
	}{
		{"replaced by another lock", func(t *testing.T, session, lock string) {
			check(t, os.Rename(lock, lock+".moved"))
			check(t, os.Mkdir(lock, 0o700))
			check(t, os.WriteFile(filepath.Join(lock, "sentinel"), []byte("untouched"), 0o600))
		}, []string{"task-one.json.lock/sentinel"}},
		{"replaced by a link", func(t *testing.T, session, lock string) {
			check(t, os.Mkdir(filepath.Join(session, "inside"), 0o700))
			check(t, os.WriteFile(filepath.Join(session, "inside", "sentinel"), []byte("untouched"), 0o600))
			check(t, os.Rename(lock, lock+".moved"))
			check(t, os.Symlink("inside", lock))
		}, []string{"task-one.json.lock", "inside/sentinel"}},
		{"gone", func(t *testing.T, session, lock string) { check(t, os.RemoveAll(lock)) }, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dispatchLockMutexHomes(t)
			dir := dispatchLockMutexOpen(t, t.TempDir(), nil)
			release := must(dir.lock("task-one.json"))
			tc.swap(t, dir.path, filepath.Join(dir.path, "task-one.json.lock"))
			if err := release(); err == nil || !strings.Contains(err.Error(), "nothing was removed") {
				t.Fatalf("release of a lock that is not the one that was created: %v", err)
			}
			for _, kept := range tc.kept {
				if !lockClearExists(filepath.Join(dir.path, kept)) {
					t.Errorf("the release removed %s", kept)
				}
			}
		})
	}
}

// A lock-clears.jsonl that is another name of a dispatch record would put the audit line into that record, so the clear
// refuses it and removes nothing; with the record gone the same clear goes through.
func TestDispatchLockClearRefusesALogWithOtherLinks(t *testing.T) {
	dispatchLockMutexHomes(t)
	ws := t.TempDir()
	env, _ := home(t)
	lock := lockClearHold(t, ws, "task-one.json")
	lockClearOwn(t, lock, lockClearDead(t))
	record := filepath.Join(lockClearDir(ws), "task-two.json")
	const body = "{\"dispatchId\":\"task-two\"}\n"
	check(t, os.WriteFile(record, []byte(body), 0o600))
	check(t, os.Link(record, filepath.Join(lockClearDir(ws), dispatchLockClearLog)))
	run := func() (int, string) {
		code, out, _ := lockClearRun(t, env, ws, "--session", lockClearSession, "--dispatch", "task-one", "--reason", "owner crashed")
		return code, out
	}
	code, out := run()
	if message, _ := lockClearLine(t, out)["error"].(string); code != 1 || !strings.Contains(message, "exactly one link") {
		t.Errorf("clear with a log that has another link: exit %d, %q", code, out)
	}
	if got := string(must(os.ReadFile(record))); got != body {
		t.Errorf("the dispatch record sharing the log's inode changed: %q", got)
	}
	if !lockClearExists(lock) {
		t.Error("the refused clear removed the lock")
	}
	check(t, os.Remove(record))
	if code, out = run(); code != 0 || lockClearExists(lock) {
		t.Errorf("clear once the log has one link: exit %d, %q", code, out)
	}
}
