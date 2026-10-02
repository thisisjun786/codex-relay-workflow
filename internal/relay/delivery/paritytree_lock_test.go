package delivery

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The tests here use a capture module of their own, so they never meet the locks or trees of a
// real run on the same machine; the lock files and the root they make are removed at the end.
func lockTestModule(t *testing.T) string {
	t.Helper()
	module := fmt.Sprintf("lock-test/%d/%s", os.Getpid(), t.Name())
	moduleKey := "capture/" + module
	t.Cleanup(func() {
		for _, key := range []string{moduleKey, moduleKey + "/a", moduleKey + "/b"} {
			_ = os.Remove(parityLockPath(key))
		}
		_ = os.Remove(filepath.Join(parityRoot, parityDigest(moduleKey)))
	})
	return module
}

// settles runs fn on a goroutine and reports whether it returned within d; wait blocks until it
// has returned, as it must once what it waited for is released.
func settles(d time.Duration, fn func()) (settled bool, wait func()) {
	var done sync.WaitGroup
	done.Add(1)
	finished := make(chan struct{})
	go func() {
		defer done.Done()
		fn()
		close(finished)
	}()
	select {
	case <-finished:
		return true, done.Wait
	case <-time.After(d):
		return false, done.Wait
	}
}

const blockedFor, settleWithin = 300 * time.Millisecond, 10 * time.Second

// Two tests of one capture module, in this process or another, hold their trees at the same
// time: what they wait for is the same test, never the module.
func TestCaptureTrees_ofOneModuleAreHeldTogether(t *testing.T) {
	module := lockTestModule(t)
	rootA, treeA, releaseA, err := lockCaptureTree(module, "a")
	mustDo(t, err)
	defer releaseA()
	var rootB, treeB string
	var releaseB func()
	settled, wait := settles(settleWithin, func() {
		rootB, treeB, releaseB, err = lockCaptureTree(module, "b")
	})
	if !settled {
		t.Fatal("a second test of the module waited for the first one's tree")
	}
	wait()
	mustDo(t, err)
	defer releaseB()
	if rootA != rootB || filepath.Dir(treeA) != rootA || filepath.Dir(treeB) != rootB || treeA == treeB {
		t.Fatalf("trees %s and %s under roots %s and %s", treeA, treeB, rootA, rootB)
	}
}

// A tree another holder has (another process holding it is another open file description, as the
// holder here is) is waited for, and is empty when it is got: that is the exclusion the goldens'
// fixed paths need.
func TestCaptureTree_waitsForAnotherHolderOfTheSameTree(t *testing.T) {
	module := lockTestModule(t)
	treeKey := "capture/" + module + "/a"
	root := filepath.Join(parityRoot, parityDigest("capture/"+module))
	other, err := lockParityTree(treeKey, filepath.Join(root, "a"))
	mustDo(t, err)
	mustDo(t, os.WriteFile(filepath.Join(root, "a", "left-behind"), nil, 0o600))
	var release func()
	var tree string
	settled, wait := settles(blockedFor, func() {
		_, tree, release, err = lockCaptureTree(module, "a")
	})
	if settled {
		other()
		t.Fatal("the tree was got while another holder had it")
	}
	other()
	wait()
	mustDo(t, err)
	defer release()
	if _, err := os.Stat(filepath.Join(tree, "left-behind")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("the tree was not emptied for its new test: %v", err)
	}
}

// An earlier revision of this harness held the module's lock file exclusively for a whole process
// and emptied the root at both ends. A capture tree waits for such a holder and, once it has one,
// keeps that holder from taking the root.
func TestCaptureTree_andAnEarlierRevisionsExclusiveRootExcludeEachOther(t *testing.T) {
	module := lockTestModule(t)
	rootLock := parityLockPath("capture/" + module)
	earlier, err := lockFile(rootLock, syscall.LOCK_EX)
	mustDo(t, err)
	var release func()
	settled, wait := settles(blockedFor, func() {
		_, _, release, err = lockCaptureTree(module, "a")
	})
	if settled {
		unlockFile(earlier)
		t.Fatal("a tree was got under a root another holder had exclusively")
	}
	unlockFile(earlier)
	wait()
	mustDo(t, err)
	defer release()
	probe, err := os.OpenFile(rootLock, os.O_RDWR, 0)
	mustDo(t, err)
	defer probe.Close()
	if err := syscall.Flock(int(probe.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); !errors.Is(err, syscall.EWOULDBLOCK) {
		t.Fatalf("an exclusive holder took the root while a tree was in use: %v", err)
	}
}

// A test that asks for a tree this process already holds would wait for itself: it is told.
func TestCaptureTree_refusesATreeThisProcessAlreadyHolds(t *testing.T) {
	module := lockTestModule(t)
	_, _, release, err := lockCaptureTree(module, "a")
	mustDo(t, err)
	settled, wait := settles(settleWithin, func() { _, _, _, err = lockCaptureTree(module, "a") })
	if !settled {
		release()
		t.Fatal("a second request for a held tree waited instead of being refused")
	}
	wait()
	if err == nil {
		t.Fatal("a second request for a held tree was granted")
	}
	release()
	_, _, again, err := lockCaptureTree(module, "a")
	mustDo(t, err)
	again()
}
