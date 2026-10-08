package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
