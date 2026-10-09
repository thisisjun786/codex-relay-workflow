package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// C6: the work on a temp root runs while the lock on the directory that holds it is held. A holder of that lock
// keeps the work waiting until it lets go.
func TestTempRootLockCRW980WaitsForTheHolderOfTheParent(t *testing.T) {
	base := t.TempDir()
	holderDir := filepath.Join(base, "tmp")
	if err := os.Mkdir(holderDir, 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(holderDir)
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	tempRoot := filepath.Join(holderDir, "crw")
	ran := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- underParentLock(tempRoot, func() error {
			close(ran)
			return nil
		})
	}()
	select {
	case <-ran:
		t.Fatal("the work ran while another holder had the lock on the directory that holds the temp root")
	case <-time.After(200 * time.Millisecond):
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the work failed after the holder released the lock: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the work did not run after the holder released the lock")
	}
}

// C6: a filesystem that refuses flock is run without the lock; a lock that is merely busy is not.
func TestTempRootLockCRW980RefusedLocksProceedUnlocked(t *testing.T) {
	for _, err := range []error{unix.ENOLCK, unix.EOPNOTSUPP, unix.ENOSYS, unix.EINVAL} {
		if !tempRootLockUnsupported(err) {
			t.Errorf("%v is a refused lock, the walk must run unlocked", err)
		}
	}
	if tempRootLockUnsupported(unix.EWOULDBLOCK) || tempRootLockUnsupported(unix.EINTR) {
		t.Error("a busy or interrupted lock is not a refused lock")
	}
}

// C6, a parent this process may only write and search: the install still runs, unlocked, as it did before the lock.
func TestTempRootLockCRW980UnreadableParentStillInstalls(t *testing.T) {
	base := t.TempDir()
	drop := filepath.Join(base, "drop")
	if err := os.Mkdir(drop, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(drop, 0o700) })
	tempRoot := filepath.Join(drop, "downloads")

	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)

	body, err := fetch(context.Background(), pin, &Seams{URLBase: release.server.URL}, tempRoot)
	if err != nil {
		t.Fatalf("fetch under a write and search only parent: %v", err)
	}
	if string(body) != "gitleaks\n" {
		t.Fatalf("the executable member is %q", body)
	}
	if _, err := os.Lstat(tempRoot); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the temp root this call made survived its cleanup: %v", err)
	}
}

// C7, d2: an ancestor the walk found is removed by a concurrent install before the walk reaches it. The walk finds
// the missing components again and makes them, so the install does not fail.
func TestTempRootLockCRW980ParentVanishingIsRecomputed(t *testing.T) {
	base := t.TempDir()
	parent := filepath.Join(base, "a")
	target := filepath.Join(parent, "b")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	removed := false
	review809Seam(t, func(path string) {
		if path == target && !removed {
			removed = true
			if err := os.Remove(parent); err != nil {
				t.Errorf("the seam could not remove the parent: %v", err)
			}
		}
	})

	created, err := createRoot(target)
	if err != nil {
		t.Fatalf("createRoot after its parent vanished: %v", err)
	}
	if !removed {
		t.Fatal("the seam did not remove the parent")
	}
	if _, found := review836RecordFor(created, target); !found {
		t.Fatalf("createRoot recorded %v, want %s", created, target)
	}
	removeCreated(created)
	if _, err := os.Lstat(target); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s survived the cleanup: %v", target, err)
	}
}

// C7: installs that share one missing temp root all succeed, and none removes a directory another is using.
func TestTempRootLockCRW980ConcurrentInstallsShareAMissingRoot(t *testing.T) {
	tree := newTestTree(t)
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)

	const installs = 6
	codes := make([]int, installs)
	outputs := make([]string, installs)
	errs := make([]string, installs)
	var wg sync.WaitGroup
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outputs[i], errs[i] = runTools(t, context.Background(), tree, &Seams{URLBase: release.server.URL}, "install", "gitleaks")
		}(i)
	}
	wg.Wait()
	for i := range codes {
		if codes[i] != 0 || errs[i] != "" {
			t.Fatalf("install %d: exit %d stdout %q stderr %q", i, codes[i], outputs[i], errs[i])
		}
	}
	if _, err := os.Stat(pin.ExecutablePath(tree.toolsRoot)); err != nil {
		t.Fatalf("the pinned executable is not installed: %v", err)
	}
}

// openFDCount is the number of descriptors this process holds.
func openFDCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("no /proc/self/fd: %v", err)
	}
	return len(entries)
}

// CRW-1045 (1): when the path names another directory after every lock is granted, openLockedDir opens and
// locks again exactly tempRootLockAttempts times and then answers a host failure. It holds no descriptor and no
// lock afterwards.
func TestTempRootLockCRW1045AttemptsAreBoundedAndLeaveNothingHeld(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "parent")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	attempts := 0
	saved := tempRootAfterLock
	t.Cleanup(func() { tempRootAfterLock = saved })
	var locked []string
	tempRootAfterLock = func(p string) {
		attempts++
		// The directory the lock was taken on is moved away and another one takes the name.
		old := filepath.Join(base, "old", string(rune('a'+attempts)))
		if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
			t.Error(err)
			return
		}
		if err := os.Rename(p, old); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Error(err)
		}
		locked = append(locked, old)
	}

	before := openFDCount(t)
	dir, held, err := openLockedDir(path)
	if dir != nil || held {
		t.Fatalf("openLockedDir kept a descriptor (%v) or a lock (%v) after a path that never held still", dir, held)
	}
	var failed *failure
	if !errors.As(err, &failed) || failed.status != notInstalledExit || !strings.Contains(failed.detail, "changed on each of 8 attempts") {
		t.Fatalf("the failure is %v, want a host failure naming the 8 attempts", err)
	}
	if attempts != tempRootLockAttempts || tempRootLockAttempts != 8 {
		t.Fatalf("openLockedDir locked %d times, want %d (the bound is 8)", attempts, tempRootLockAttempts)
	}
	if after := openFDCount(t); after != before {
		t.Errorf("the process holds %d descriptors after the failure, %d before", after, before)
	}
	// No lock stays on any directory that was locked: a non-blocking lock succeeds on each.
	for _, old := range locked {
		probe, err := os.Open(old)
		if err != nil {
			t.Fatal(err)
		}
		if err := unix.Flock(int(probe.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Errorf("%s is still locked after the failure: %v", old, err)
		}
		probe.Close()
	}
}

// CRW-1045 (1) contrast: a path that changes fewer times than the bound is locked at last.
func TestTempRootLockCRW1045ASettledPathIsLockedAfterAChange(t *testing.T) {
	base := t.TempDir()
	path := filepath.Join(base, "parent")
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	changes := 0
	saved := tempRootAfterLock
	t.Cleanup(func() { tempRootAfterLock = saved })
	tempRootAfterLock = func(p string) {
		if changes >= tempRootLockAttempts-1 {
			return
		}
		changes++
		if err := os.Rename(p, filepath.Join(base, "old"+string(rune('a'+changes)))); err != nil {
			t.Error(err)
			return
		}
		if err := os.Mkdir(p, 0o755); err != nil {
			t.Error(err)
		}
	}
	dir, held, err := openLockedDir(path)
	if err != nil || dir == nil || !held {
		t.Fatalf("openLockedDir = %v, %v, %v after %d changes", dir, held, err, changes)
	}
	dir.Close()
	if changes != tempRootLockAttempts-1 {
		t.Fatalf("the path changed %d times, want %d", changes, tempRootLockAttempts-1)
	}
}

// CRW-1045 (2): a filesystem whose flock answers ENOLCK, EOPNOTSUPP, ENOSYS or EINVAL runs the walk unlocked: openLockedDir
// answers the descriptor without a lock, and a whole fetch succeeds. Any other error is a host failure.
func TestTempRootLockCRW1045RefusedFlockInstallsUnlocked(t *testing.T) {
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)

	for _, refused := range []error{unix.ENOLCK, unix.EOPNOTSUPP, unix.ENOSYS, unix.EINVAL} {
		t.Run(refused.Error(), func(t *testing.T) {
			calls := 0
			saved := tempRootFlock
			t.Cleanup(func() { tempRootFlock = saved })
			tempRootFlock = func(int, int) error { calls++; return refused }

			parent := filepath.Join(t.TempDir(), "tmp")
			if err := os.Mkdir(parent, 0o755); err != nil {
				t.Fatal(err)
			}
			dir, held, err := openLockedDir(parent)
			if err != nil || dir == nil || held {
				t.Fatalf("openLockedDir = %v, %v, %v, want a descriptor with no lock and no error", dir, held, err)
			}
			dir.Close()

			tempRoot := filepath.Join(parent, "a", "b")
			body, err := fetch(context.Background(), pin, &Seams{URLBase: release.server.URL}, tempRoot)
			if err != nil || string(body) != "gitleaks\n" {
				t.Fatalf("fetch under a filesystem that refuses flock: %q, %v", body, err)
			}
			if calls < 2 {
				t.Errorf("flock was asked %d times, the refusal was never reached by the fetch", calls)
			}
			if _, err := os.Lstat(filepath.Join(parent, "a")); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the temp root this call made survived its cleanup: %v", err)
			}
		})
	}

	t.Run("another error is a host failure", func(t *testing.T) {
		saved := tempRootFlock
		t.Cleanup(func() { tempRootFlock = saved })
		tempRootFlock = func(int, int) error { return unix.EIO }
		parent := filepath.Join(t.TempDir(), "tmp")
		if err := os.Mkdir(parent, 0o755); err != nil {
			t.Fatal(err)
		}
		if dir, _, err := openLockedDir(parent); err == nil || dir != nil {
			t.Fatalf("openLockedDir answered %v, %v for an EIO lock", dir, err)
		}
		tempRoot := filepath.Join(parent, "a", "b")
		_, err := fetch(context.Background(), pin, &Seams{URLBase: release.server.URL}, tempRoot)
		var failed *failure
		if !errors.As(err, &failed) || failed.status != notInstalledExit {
			t.Fatalf("fetch with an EIO lock = %v, want a host failure", err)
		}
		if _, err := os.Lstat(filepath.Join(parent, "a")); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("the failed fetch left its temp root: %v", err)
		}
	})
}

// CRW-1045 (3): two installs on one missing temp root are ordered by the lock on the directory that holds it.
// The first is held inside its locked mkdir; the second must not reach its own mkdir until the first lets go.
// The barrier replaces the timing the old test relied on: without the lock the second reaches its mkdir at
// once and the test fails, with it the second waits and both installs succeed.
func TestTempRootLockCRW1045ConcurrentInstallsAreOrderedByTheLock(t *testing.T) {
	archive := syntheticArchive(t, []byte("gitleaks\n"))
	sum := sha256.Sum256(archive)
	pin := testPin(hex.EncodeToString(sum[:]))
	withPin(t, pin)
	release := newFakeRelease(t, archive)
	parent := filepath.Join(t.TempDir(), "tmp")
	if err := os.Mkdir(parent, 0o755); err != nil {
		t.Fatal(err)
	}
	tempRoot := filepath.Join(parent, "crw")

	// An install whose temp root was removed under it tries its mkdir again, so a hook may run twice.
	firstInside, firstGo, secondInside := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var firstOnce, secondOnce sync.Once
	first := &Seams{URLBase: release.server.URL, MkdirTemp: func(dir, pattern string) (string, error) {
		firstOnce.Do(func() {
			close(firstInside)
			<-firstGo
		})
		return os.MkdirTemp(dir, pattern)
	}}
	second := &Seams{URLBase: release.server.URL, MkdirTemp: func(dir, pattern string) (string, error) {
		secondOnce.Do(func() { close(secondInside) })
		return os.MkdirTemp(dir, pattern)
	}}
	type result struct {
		body []byte
		err  error
	}
	results := make(chan result, 2)
	go func() {
		body, err := fetch(context.Background(), pin, first, tempRoot)
		results <- result{body, err}
	}()
	select {
	case <-firstInside:
	case <-time.After(10 * time.Second):
		t.Fatal("the first install never reached its locked mkdir")
	}
	go func() {
		body, err := fetch(context.Background(), pin, second, tempRoot)
		results <- result{body, err}
	}()
	select {
	case <-secondInside:
		close(firstGo)
		t.Fatal("the second install reached its mkdir while the first held the lock on the directory that holds the temp root")
	case <-time.After(500 * time.Millisecond):
	}
	close(firstGo)
	for range 2 {
		select {
		case r := <-results:
			if r.err != nil || string(r.body) != "gitleaks\n" {
				t.Fatalf("an install failed: %q, %v", r.body, r.err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("an install did not finish")
		}
	}
	select {
	case <-secondInside:
	default:
		t.Fatal("the second install never made its directory")
	}
	if entries, err := os.ReadDir(parent); err == nil {
		for _, entry := range entries {
			if entry.Name() == "crw" {
				if left, _ := os.ReadDir(tempRoot); len(left) != 0 {
					t.Errorf("a download directory survived under the temp root: %v", left)
				}
			}
		}
	}
}
