package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// C6: the temp root's ancestor is locked for the whole creation. An install that waits for the lock on
// that ancestor makes nothing until the holder releases it.
func TestTempRootLockCRW980WaitsForTheAncestorLock(t *testing.T) {
	base := t.TempDir()
	tempRoot := filepath.Join(base, "tmp", "crw")
	if err := os.Mkdir(filepath.Join(base, "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	holder, err := os.Open(filepath.Join(base, "tmp"))
	if err != nil {
		t.Fatal(err)
	}
	defer holder.Close()
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	lock := newTempRootLock(tempRoot)
	done := make(chan error, 1)
	go func() { done <- lock.acquire() }()
	select {
	case err := <-done:
		t.Fatalf("the lock was granted while another holder had it: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	if err := unix.Flock(int(holder.Fd()), unix.LOCK_UN); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("acquire after the holder released: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the lock was not granted after the holder released it")
	}
	lock.release()
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
