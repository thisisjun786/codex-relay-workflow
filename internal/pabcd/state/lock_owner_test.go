package state

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// CRW-1094 (isolated trial S4-F2; known-defects.md:76 and :77): a lock left by a process that died is
// taken over once its owner is known to be gone, a live owner's lock never is, a lock whose owner record
// cannot be written is not left behind, and the release removes only the lock this holder made.

// lockOwnerDeadPid is the pid of a child that has exited and been reaped.
func lockOwnerDeadPid(t *testing.T) int {
	t.Helper()
	cmd := exec.Command("true")
	if err := cmd.Run(); err != nil {
		t.Fatal(err)
	}
	return cmd.Process.Pid
}

func lockOwnerSeed(t *testing.T, cwd, content string) string {
	t.Helper()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lock := StatePath(cwd, "s") + ".lock"
	if err := os.WriteFile(lock, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return lock
}

// A lock left behind is taken over on the first attempt, without a wait, when its owner is gone: the
// oracle's pid record of a dead process, this protocol's record of a dead holder, and the record of a
// holder that died inside its critical section, which leaves the file but not the kernel lock.
func TestALockWhoseOwnerIsGoneIsTakenOverAtOnce(t *testing.T) {
	for name, content := range map[string]string{
		"a dead pid":             strconv.Itoa(lockOwnerDeadPid(t)),
		"a dead holder's record": sessionLockRecord(lockOwnerDeadPid(t)),
	} {
		cwd := t.TempDir()
		lock := lockOwnerSeed(t, cwd, content)
		entered, slept := false, 0
		err := withSessionLock(cwd, "s", func() error { entered = true; return nil }, func(time.Duration) { slept++ })
		if err != nil || !entered || slept != 0 {
			t.Fatalf("%s: err %v entered %v slept %d; want the stale lock taken over at once", name, err, entered, slept)
		}
		if _, err := os.Lstat(lock); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("%s: the lock was not released: %v", name, err)
		}
	}
	cwd := t.TempDir()
	children(t, cwd, [2]string{"die-holding", ""})
	lock := StatePath(cwd, "conc") + ".lock"
	if _, err := os.Lstat(lock); err != nil {
		t.Fatalf("the killed holder left no lock to take over: %v", err)
	}
	entered, slept := false, 0
	if err := withSessionLock(cwd, "conc", func() error { entered = true; return nil }, func(time.Duration) { slept++ }); err != nil || !entered || slept != 0 {
		t.Fatalf("the lock of a killed holder: err %v entered %v slept %d", err, entered, slept)
	}
}

// A live owner is never taken over: a holder in this process, a live pid in the oracle's record, and a
// record whose owner cannot be judged all wait out the schedule and fail with the busy error, leaving
// the file as it was.
func TestALiveOrUnknownOwnersLockIsNeverTakenOver(t *testing.T) {
	cwd := t.TempDir()
	held, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- WithSessionLock(cwd, "s", func() error { close(held); <-release; return nil })
	}()
	<-held
	entered := false
	err := withSessionLock(cwd, "s", func() error { entered = true; return nil }, func(time.Duration) {})
	if !errors.Is(err, fs.ErrExist) || entered {
		t.Fatalf("a live holder in this process: err %v entered %v", err, entered)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{"a live pid": strconv.Itoa(os.Getpid()), "pid 1": "1", "an unknown record": "held", "an empty record": ""} {
		cwd := t.TempDir()
		lock := lockOwnerSeed(t, cwd, content)
		err := withSessionLock(cwd, "s", func() error { entered = true; return nil }, func(time.Duration) {})
		if !errors.Is(err, fs.ErrExist) || entered || fileText(t, lock) != content {
			t.Fatalf("%s: err %v entered %v lock %q", name, err, entered, fileText(t, lock))
		}
	}
}

// The oracle's holder creates its lock file exclusively and writes its pid afterwards, holding no kernel lock, so between the two
// calls a live holder's file is empty. A holder paused in that window (here: the file made as the oracle makes it, its descriptor
// still open) is never taken over: the new caller's critical section does not run and the attempt ends with the busy error.
func TestALiveOracleHolderBeforeItsPidWriteIsNeverTakenOver(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	lock := SessionLockPath(cwd, "s")
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666) // the oracle's "wx" create, before its pid write
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	entered, slept := false, 0
	err = withSessionLock(cwd, "s", func() error { entered = true; return nil }, func(time.Duration) { slept++ })
	if !errors.Is(err, fs.ErrExist) || entered || slept == 0 || fileText(t, lock) != "" {
		t.Fatalf("err %v entered %v slept %d lock %q; want the busy error after the schedule and the file untouched", err, entered, slept, fileText(t, lock))
	}
}

// This protocol never shows an empty record at the lock path, so an empty one can only be an oracle holder's: the record is
// written into a file that the path does not name yet, which is then linked into place whole; a takeover renames a complete record
// over the dead holder's file, so the path never names an empty file in between.
func TestTheLockPathNeverNamesAnEmptyRecord(t *testing.T) {
	saved := sessionLockWriteRecord
	t.Cleanup(func() { sessionLockWriteRecord = saved })
	for name, seed := range map[string]*string{"a fresh lock": nil, "a takeover": func() *string { r := sessionLockRecord(lockOwnerDeadPid(t)); return &r }()} {
		cwd := t.TempDir()
		lock := SessionLockPath(cwd, "s")
		if seed != nil {
			lockOwnerSeed(t, cwd, *seed)
		}
		var seen []string
		sessionLockWriteRecord = func(f *os.File, record string) error {
			text, err := os.ReadFile(lock)
			switch {
			case errors.Is(err, fs.ErrNotExist):
				seen = append(seen, "absent")
			case err != nil:
				seen = append(seen, err.Error())
			default:
				seen = append(seen, string(text))
			}
			return saved(f, record)
		}
		entered := ""
		if err := WithSessionLock(cwd, "s", func() error { entered = fileText(t, lock); return nil }); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := "absent"
		if seed != nil {
			want = *seed
		}
		if len(seen) != 1 || seen[0] != want || entered != sessionLockRecord(os.Getpid()) {
			t.Fatalf("%s: while the record was written the path held %q (want %q); inside, %q", name, seen, want, entered)
		}
		if names := sessionFiles(cwd); len(names) != 0 {
			t.Fatalf("%s: left %v in the sessions directory", name, names)
		}
	}
}

// Processes that contend for one lock, through its file being created, taken, released and removed
// over and over, never lose an update: the lock alone decides who is inside.
func TestProcessesContendingForTheLockLoseNoUpdate(t *testing.T) {
	cwd := t.TempDir()
	children(t, cwd, [2]string{"inc", ""}, [2]string{"inc", ""}, [2]string{"inc", ""}, [2]string{"inc", ""})
	if got := ReadState(cwd, "conc").IdleEditNudges; got != 60 {
		t.Fatalf("%v updates survived of 60", got)
	}
	if _, err := os.Lstat(StatePath(cwd, "conc") + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("lock left behind: %v", err)
	}
}

// known-defects.md:76: the oracle creates the lock file and then writes the pid, so a failed write
// leaves an empty lock. The lock file this call made is removed again, and fn never runs.
func TestALockWhoseOwnerRecordCannotBeWrittenIsNotLeftBehind(t *testing.T) {
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	out := strings.TrimSpace(children(t, cwd, [2]string{"efbig-lock", ""})[0])
	if strings.HasPrefix(out, "setup:") {
		t.Skipf("cannot lower RLIMIT_FSIZE here: %s", out)
	}
	if out != "ran=false efbig=true" {
		t.Fatalf("the child answered %q", out)
	}
	if _, err := os.Lstat(StatePath(cwd, "conc") + ".lock"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a lock whose owner record failed was left behind: %v", err)
	}
}

// known-defects.md:77: the oracle releases by path, so a lock another process made after this one's
// file was removed by hand is deleted by this holder's release. The release removes its own file only.
func TestTheReleaseKeepsALockThatReplacedItsOwn(t *testing.T) {
	cwd := t.TempDir()
	lock := StatePath(cwd, "s") + ".lock"
	err := WithSessionLock(cwd, "s", func() error {
		if err := os.Remove(lock); err != nil {
			return err
		}
		return os.WriteFile(lock, []byte("another holder"), 0o644)
	})
	if err != nil || fileText(t, lock) != "another holder" {
		t.Fatalf("err %v; the replacing lock holds %q", err, fileText(t, lock))
	}
}

// Many holders in one process take turns on the lock without overlapping.
func TestGoroutinesTakeTurnsOnTheLock(t *testing.T) {
	cwd := t.TempDir()
	var inside, overlaps int
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 5; j++ {
				if err := withSessionLock(cwd, "s", func() error {
					mu.Lock()
					inside++
					if inside > 1 {
						overlaps++
					}
					mu.Unlock()
					time.Sleep(100 * time.Microsecond)
					mu.Lock()
					inside--
					mu.Unlock()
					return nil
				}, lockBudgetSleep()); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	if overlaps != 0 {
		t.Fatalf("%d overlapping critical sections", overlaps)
	}
}

// createExclusive is writeFileSync(path, data, { flag: "wx" }), the oracle's lock create, which the tests use to put a lock file
// in place the way the oracle's holder did.
func createExclusive(path, data string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o666)
	if err != nil {
		return err
	}
	_, err = f.WriteString(data)
	return errors.Join(err, f.Close())
}

// lockOwnerLive is the oracle's record of a holder that is alive: this test process's own pid. A test that needs a lock file
// nobody takes over uses it; the oracle's tests used any number, which since CRW-1094 is taken over when no such process exists.
func lockOwnerLive() string { return strconv.Itoa(os.Getpid()) }

// CRW-1094 (pre-merge evaluation d1): a filesystem or seccomp policy that answers EPERM, ENOTSUP, EINVAL, ENOSYS or EOPNOTSUPP to the
// no-replace rename must not stop a fresh lock: the lock is then published with the exclusive hard link, as EnsureState publishes a
// state file (noReplaceUnsupported), and a lock another acquirer put at the path first is still refused as busy.
func TestAFreshLockFallsBackToALinkWhereTheNoReplaceRenameIsRefused(t *testing.T) {
	for _, errno := range []syscall.Errno{syscall.EPERM, syscall.ENOTSUP, syscall.EINVAL, syscall.ENOSYS, syscall.EOPNOTSUPP} {
		real := sessionLockRenameNoReplace
		sessionLockRenameNoReplace = func(oldpath, newpath string) error {
			return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: errno}
		}
		cwd := t.TempDir()
		entered := false
		err := WithSessionLock(cwd, "s", func() error {
			entered = true
			raw, rerr := os.ReadFile(StatePath(cwd, "s") + ".lock")
			if rerr != nil || string(raw) != sessionLockRecord(os.Getpid()) {
				t.Errorf("%v: the lock at the path holds %q (%v); want this holder's record", errno, raw, rerr)
			}
			return nil
		})
		sessionLockRenameNoReplace = real
		if err != nil || !entered {
			t.Fatalf("%v: err %v entered %v; want the lock published through a link", errno, err, entered)
		}
		entries, rerr := os.ReadDir(filepath.Dir(StatePath(cwd, "s")))
		if rerr != nil || len(entries) != 0 {
			t.Fatalf("%v: the sessions directory holds %v (%v) after the release; want nothing", errno, entries, rerr)
		}
	}
}

// Where the rename is refused, a lock already at the path is still the busy answer, never a takeover or a second holder.
func TestTheLinkFallbackOfAFreshLockStillRefusesAnOccupiedPath(t *testing.T) {
	real := sessionLockRenameNoReplace
	sessionLockRenameNoReplace = func(oldpath, newpath string) error {
		if err := os.WriteFile(newpath, []byte(sessionLockRecord(os.Getpid())), 0o644); err != nil { // another acquirer was first
			t.Error(err)
		}
		return &os.LinkError{Op: "rename", Old: oldpath, New: newpath, Err: syscall.EPERM}
	}
	defer func() { sessionLockRenameNoReplace = real }()
	cwd := t.TempDir()
	if err := makeSessionsDir(cwd); err != nil {
		t.Fatal(err)
	}
	held, err := placeSessionLock(StatePath(cwd, "s")+".lock", false)
	if held != nil || !errors.Is(err, fs.ErrExist) {
		t.Fatalf("held %v err %v; want fs.ErrExist and no lock", held, err)
	}
}
