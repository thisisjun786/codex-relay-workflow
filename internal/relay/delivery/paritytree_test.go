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
// fixed directory under parityRoot rather than a fresh temporary one. The directory is named by a
// digest of the test's name (the fixed trees began as the paths the Python reference runs used),
// locked (flock) for as long as the test uses it so two test processes never share one, and
// written into a golden as "<tree>".
const parityRoot = "/tmp/crw-delivery-parity"

var (
	parityMu    sync.Mutex
	parityCount = map[string]int{}
	// parityHeld are the trees locked for the whole process (a capture module's root).
	parityHeld = map[string]string{}
)

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
	dir, release, err := lockParityDir(key)
	mustDo(t, err)
	t.Cleanup(release)
	return dir
}

// processParityTree is the fixed tree for key, locked until the test process exits (TestMain
// releases it). A capture module's Python run writes every test's tree under one such root.
func processParityTree(t testing.TB, key string) string {
	t.Helper()
	parityMu.Lock()
	defer parityMu.Unlock()
	if dir, ok := parityHeld[key]; ok {
		return dir
	}
	dir, release, err := lockParityDir(key)
	if err != nil {
		t.Fatal(err)
	}
	parityHeld[key] = dir
	parityReleases = append(parityReleases, release)
	return dir
}

var parityReleases []func()

// releaseParityTrees removes and unlocks every process-lifetime tree.
func releaseParityTrees() {
	for _, release := range parityReleases {
		release()
	}
}

func lockParityDir(key string) (string, func(), error) {
	sum := sha256.Sum256([]byte(key))
	dir := filepath.Join(parityRoot, hex.EncodeToString(sum[:])[:12])
	if err := os.MkdirAll(parityRoot, 0o755); err != nil {
		return "", nil, err
	}
	lock, err := os.OpenFile(dir+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return "", nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		_ = lock.Close()
		return "", nil, err
	}
	release := func() {
		_ = testsupport.RemoveTempTree(dir)
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		_ = lock.Close()
	}
	if err := testsupport.RemoveTempTree(dir); err != nil {
		release()
		return "", nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		release()
		return "", nil, err
	}
	return dir, release, nil
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
