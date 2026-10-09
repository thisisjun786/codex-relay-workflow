package state

import (
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
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
// oracle's pid record of a dead process, an empty record (a pid write that failed), and the record of a
// holder that died inside its critical section, which leaves the file but not the kernel lock.
func TestALockWhoseOwnerIsGoneIsTakenOverAtOnce(t *testing.T) {
	for name, content := range map[string]string{
		"a dead pid":   strconv.Itoa(lockOwnerDeadPid(t)),
		"empty record": "",
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
	for name, content := range map[string]string{"a live pid": strconv.Itoa(os.Getpid()), "pid 1": "1", "an unknown record": "held"} {
		cwd := t.TempDir()
		lock := lockOwnerSeed(t, cwd, content)
		err := withSessionLock(cwd, "s", func() error { entered = true; return nil }, func(time.Duration) {})
		if !errors.Is(err, fs.ErrExist) || entered || fileText(t, lock) != content {
			t.Fatalf("%s: err %v entered %v lock %q", name, err, entered, fileText(t, lock))
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
