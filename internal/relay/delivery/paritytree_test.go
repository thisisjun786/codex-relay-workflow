package delivery

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// Many goldens of this package hold values derived from absolute fixture paths: an artifact's
// declared path is part of the manifest revision hash, which is part of the event id, which is
// part of the request id and every row that names it; a workspace key hashes the workspace's
// path, and a fault observation's evidence digest names the installation directory. Such a golden
// can only be checked against a run that declares the same paths, so the tree of such a test is a
// fixed directory under parityRoot rather than a fresh temporary one, and the same path for every
// checkout: a root per checkout would move every one of those values. The directory is named by
// a digest of the test's name (the fixed trees began as the paths the Python reference runs
// used), locked (flock) for as long as the test uses it so two test processes never share one,
// and written into a golden as "<tree>".
//
// A lock lasts for one test, never for a process: a process that held its trees until it exited
// made every other checkout testing this package on the machine wait for that whole run. Two
// processes now wait for each other only while both are inside the same test.
const parityRoot = "/tmp/crw-delivery-parity"

var (
	parityMu    sync.Mutex
	parityCount = map[string]int{}
	// captureHeld are the capture trees (lockCaptureTree) this process holds now, by tree key.
	captureHeld = map[string]bool{}
)

// parityDigest names a key's directory and lock file: the first twelve hex digits of its sha256.
func parityDigest(key string) string {
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])[:12]
}

// parityLockPath is the lock file of key's tree. It lies beside the trees, not inside one, because
// a tree is removed and made again while its lock is held, and a lock file is never removed:
// another process may be waiting on it.
func parityLockPath(key string) string {
	return filepath.Join(parityRoot, parityDigest(key)+".lock")
}

// parityTree is the fixed tree for the calling test, empty and locked until the test ends. A test
// that asks twice gets a second tree.
func parityTree(t *testing.T) string {
	t.Helper()
	parityMu.Lock()
	n := parityCount[t.Name()]
	parityCount[t.Name()] = n + 1
	parityMu.Unlock()
	key := t.Name()
	if n > 0 {
		key = fmt.Sprintf("%s#%d", key, n)
	}
	dir := filepath.Join(parityRoot, parityDigest(key))
	release, err := lockParityTree(key, dir)
	mustDo(t, err)
	t.Cleanup(release)
	return dir
}

// lockParityTree locks key's lock file exclusively, then empties and makes dir. release removes
// dir and unlocks.
func lockParityTree(key, dir string) (release func(), err error) {
	lock, err := lockFile(parityLockPath(key), syscall.LOCK_EX)
	if err != nil {
		return nil, err
	}
	release = func() {
		_ = testsupport.RemoveTempTree(dir)
		unlockFile(lock)
	}
	if err := testsupport.RemoveTempTree(dir); err != nil {
		release()
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

// lockCaptureTree is the tree module's Class.method name runs in (hostloss_harness_test.go), and
// the root of every tree of module: both fixed paths, because the golden values hold ids derived
// from them. Only the tree is held, for one test, so tests of one module run in parallel in other
// processes. The root is not removed (nothing of a test lies in it but its tree), and its lock
// file is held shared for as long as the tree is used: an earlier revision of this file held it
// exclusively for a whole process and emptied the root at both ends, so while such a checkout
// runs on this machine this test waits for it, and it cannot empty a tree in use. The locks are
// taken root first, tree second, and released the other way round.
func lockCaptureTree(module, name string) (root, tree string, release func(), err error) {
	moduleKey := "capture/" + module
	treeKey := moduleKey + "/" + name
	root = filepath.Join(parityRoot, parityDigest(moduleKey))
	tree = filepath.Join(root, name)
	parityMu.Lock()
	held := captureHeld[treeKey]
	captureHeld[treeKey] = true
	parityMu.Unlock()
	if held {
		return "", "", nil, fmt.Errorf("capture tree %s is already in use by a test of this process; a second request would wait for it forever", treeKey)
	}
	free := func() {
		parityMu.Lock()
		delete(captureHeld, treeKey)
		parityMu.Unlock()
	}
	shared, err := lockFile(parityLockPath(moduleKey), syscall.LOCK_SH)
	if err != nil {
		free()
		return "", "", nil, err
	}
	unlockTree, err := lockParityTree(treeKey, tree)
	if err != nil {
		unlockFile(shared)
		free()
		return "", "", nil, err
	}
	return root, tree, func() {
		unlockTree()
		unlockFile(shared)
		free()
	}, nil
}

// installationDir is the installation directory the Go sweeper is given (sweeper): the fault sweep
// names it in every observation's evidence and digests that evidence, so the golden holds a digest
// of this path, which is therefore a fixed one. It began as the directory the captured Python ran
// its relay package from, and keeps that tree's key. Only the path is used: the sweeper prints it
// and resolves its symbolic links, and nothing ever made the directory, so nothing is locked.
func installationDir() string {
	return filepath.Join(parityRoot, parityDigest("python-package"), "codex_session_relay")
}

// lockFile opens the lock file at path, creating it and its directory, and waits for how
// (syscall.LOCK_SH or syscall.LOCK_EX) on it.
func lockFile(path string, how int) (*os.File, error) {
	if err := os.MkdirAll(parityRoot, 0o755); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), how); err != nil {
		_ = lock.Close()
		return nil, err
	}
	return lock, nil
}

// unlockFile releases a lock lockFile took.
func unlockFile(lock *os.File) {
	_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	_ = lock.Close()
}

// inDirectory runs fn in dir and returns to the working directory it left. golden files a golden
// relative to the package directory, so a test that must work elsewhere moves there only around its
// own calls rather than for the whole test (t.Chdir).
func inDirectory(t *testing.T, dir string, fn func()) {
	t.Helper()
	wd, err := os.Getwd()
	mustDo(t, err)
	mustDo(t, os.Chdir(dir))
	defer func() { mustDo(t, os.Chdir(wd)) }()
	fn()
}
