//go:build linux

package store

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// CRW-880: the three paths the store-file registry still left open after CRW-846.
//
// 1. holdStoreFile answered byPath, then measured the identity, then opened, and registered the
//    new descriptor unconditionally: a path that came back to an inode the registry already held
//    between the stat and the open overwrote byKey, the displaced handle became unreachable, and
//    os.File's finalizer closed it at the next collection, dropping this process's POSIX locks.
// 2. A non-regular path was opened without O_NONBLOCK while the process-wide registry mutex was
//    held, so a writerless FIFO at relay.sqlite3 stopped every store read in the process.
// 3. HashArtifact opened and closed an artifact with a plain close, so an artifact root that
//    covers the state directory let an emit read relay.sqlite3 (or a hard link to it) and drop
//    the locks of the WAL connection this process holds.
//
// The registry is process-wide shared state, so every test here runs serially (no t.Parallel)
// and touches only temporary state.

// storeFileLockLines returns the /proc/locks lines this process holds on one inode, for a
// precondition's log. An unreadable /proc/locks is no lines, which the caller reports as such.
func storeFileLockLines(pid int, inode uint64) []string {
	raw, err := os.ReadFile("/proc/locks")
	if err != nil {
		return nil
	}
	want := strconv.FormatUint(inode, 10)
	var lines []string
	for _, line := range strings.Split(string(raw), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 6 || fields[4] != strconv.Itoa(pid) {
			continue
		}
		parts := strings.Split(fields[5], ":")
		if len(parts) == 3 && parts[2] == want {
			lines = append(lines, line)
		}
	}
	return lines
}

// requireObservableLock is the precondition of a test that measures the process's POSIX locks on
// the store's main inode. A count of 0, or a lookup error (-1), before the step fails the test with
// the /proc/locks lines naming the inode (CRW-888): a skip or a log alone judges nothing, and a
// precondition that does not hold is a failed run, not a passed one.
func requireObservableLock(t *testing.T, inode uint64, before int, where string) {
	t.Helper()
	if before >= 1 {
		return
	}
	lines := storeFileLockLines(os.Getpid(), inode)
	t.Fatalf("the process holds no POSIX lock on the store's main inode %d before %s (locks=%d), so this test cannot observe the lock it means to; the /proc/locks lines naming this process and inode: %q", inode, where, before, lines)
}

// openDescriptorsUnder counts the descriptors this process holds whose target lies under root:
// what one test opened. It is the scoped form of openDescriptorCount (storefile_test.go) for a
// test that compares a count before and after its own work, so another test's descriptors - and a
// finalizer anywhere in the process - cannot move the number it judges.
func openDescriptorsUnder(t *testing.T, root string) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd is unavailable: %v", err)
	}
	count := 0
	for _, entry := range entries {
		target, err := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if err != nil {
			continue
		}
		if target == root || strings.HasPrefix(target, root+string(os.PathSeparator)) {
			count++
		}
	}
	return count
}

// withStoreFileStatHook installs the stat/open seam for one path, once.
func withStoreFileStatHook(t *testing.T, path string, act func()) {
	t.Helper()
	fired := false
	storeFileAfterStatHook = func(observed string) {
		if observed != path || fired {
			return
		}
		fired = true
		act()
	}
	t.Cleanup(func() { storeFileAfterStatHook = nil })
}

func storeFileInodeOf(t *testing.T, path string) uint64 {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		t.Fatalf("no syscall.Stat_t for %s", path)
	}
	return uint64(stat.Ino)
}

// openRaceStore opens a real store and writes one row, so this process holds a WAL connection
// and with it POSIX locks on the store's main inode. The caller closes it.
func openRaceStore(t *testing.T, path string) *Store {
	t.Helper()
	ctx := context.Background()
	s, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Querier(ctx).ExecContext(ctx, "INSERT INTO store_challenge(nonce,written_by,written_at) VALUES('race','race','2026-01-01T00:00:00Z')"); err != nil {
		_ = s.Close()
		t.Fatal(err)
	}
	return s
}

// moveAwayAndBack renames the store away and, at the seam, brings the same inode back to the
// path: the registry's stat sees ENOENT and its open lands on the inode it already holds.
func moveAwayAndBack(t *testing.T, path string) {
	t.Helper()
	away := filepath.Join(filepath.Dir(path), "moved-away.sqlite3")
	if err := os.Rename(path, away); err != nil {
		t.Fatal(err)
	}
	withStoreFileStatHook(t, path, func() {
		if err := os.Rename(away, path); err != nil {
			t.Fatal(err)
		}
	})
}

// systemSQLitePeer opens and closes the store with the system SQLite, the configuration the
// issue's reproduction used. It reports whether the peer ran.
func systemSQLitePeer(t *testing.T, path string) bool {
	t.Helper()
	python, err := exec.LookPath("python3")
	if err != nil {
		return false
	}
	script := "import sqlite3,sys;c=sqlite3.connect(sys.argv[1],timeout=30);c.execute('select count(*) from store_challenge').fetchone();c.close()"
	out, err := exec.Command(python, "-c", script, path).CombinedOutput()
	if err != nil {
		t.Fatalf("python peer: %v %s", err, out)
	}
	return true
}

// TestStoreFileRace_pathReturningToAHeldInodeKeepsTheHeldHandle is defect 1: after the open and
// fstat, the registry must answer with the handle it already holds for that (device, inode).
func TestStoreFileRace_pathReturningToAHeldInodeKeepsTheHeldHandle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()

	first, err := holdStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	moveAwayAndBack(t, path)
	second, err := holdStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("a path that came back to an inode the registry already holds got a second handle (%p != %p): the displaced handle becomes unreachable and its finalizer closes it, dropping this process's POSIX locks on the file", first, second)
	}
	if _, err := second.Stat(); err != nil {
		t.Fatalf("the reused handle is not usable: %v", err)
	}
}

// TestStoreFileRace_aDisplacedHandleSurvivesGarbageCollection proves the consequence: the
// process's POSIX lock on the store's main inode outlives two collections and a system SQLite
// peer's open and close.
func TestStoreFileRace_aDisplacedHandleSurvivesGarbageCollection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	inode := storeFileInodeOf(t, path)

	// The first handle is deliberately not kept: in the defect the registry loses it, so only
	// what the registry itself holds keeps it alive.
	if _, err := holdStoreFile(path); err != nil {
		t.Fatal(err)
	}
	before, _ := storeFileLocks(os.Getpid(), inode)
	requireObservableLock(t, inode, before, "the race")

	moveAwayAndBack(t, path)
	if _, err := holdStoreFile(path); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.GC()
	runtime.Gosched()

	// A real loss reads 0: closing any descriptor of a file drops every POSIX lock this process
	// holds on it, so a process that lost the lock holds none. A lower but non-zero count is not
	// a lost lock - the /proc/locks line count on one inode can carry a transient extra line,
	// because SQLite's lock byte ranges differ from moment to moment - so it is logged, not failed.
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "garbage collection")
	if !systemSQLitePeer(t, path) {
		t.Log("python3 is absent, so the system-SQLite-peer layer of this test did not run")
	}
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "a system SQLite peer's open and close")
}

// TestStoreFileRace_aWriterlessFifoDoesNotBlockTheRegistry is defect 2: a path that is not a
// regular file is refused without a blocking open while the registry mutex is held.
func TestStoreFileRace_aWriterlessFifoDoesNotBlockTheRegistry(t *testing.T) {
	root := t.TempDir()
	normal := filepath.Join(root, "normal")
	if err := os.MkdirAll(normal, 0o700); err != nil {
		t.Fatal(err)
	}
	normalPath := filepath.Join(normal, "relay.sqlite3")
	s, err := fixtureOpen(context.Background(), normalPath, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()

	pipe := filepath.Join(root, "pipe")
	if err := os.MkdirAll(pipe, 0o700); err != nil {
		t.Fatal(err)
	}
	pipePath := filepath.Join(pipe, "relay.sqlite3")
	if err := syscall.Mkfifo(pipePath, 0o600); err != nil {
		t.Fatal(err)
	}

	done := make(chan string, 3)
	go func() {
		_ = StoreSocket(pipePath)
		done <- "StoreSocket on the FIFO"
	}()
	go func() {
		_ = ReadOnlyRows(context.Background(), StateSelection{Path: normal}, "SELECT nonce FROM store_challenge", nil, func(RowScanner) error { return nil })
		done <- "ReadOnlyRows on a normal store"
	}()
	go func() {
		_, cleanup, _ := ownership.CopySnapshot(normalPath)
		if cleanup != nil {
			_ = cleanup()
		}
		done <- "CopySnapshot on a normal store"
	}()

	deadline := time.After(5 * time.Second)
	for range 3 {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("a store read did not finish within 5s: a writerless FIFO at relay.sqlite3 blocks open(2) while the process-wide registry mutex is held (CRW-880)")
		}
	}
}

// TestStoreFileRace_siblingDiscoverySkipsANonRegularDatabase is the discovery half of defect 2.
func TestStoreFileRace_siblingDiscoverySkipsANonRegularDatabase(t *testing.T) {
	root := t.TempDir()
	normal := filepath.Join(root, "normal")
	if err := os.MkdirAll(normal, 0o700); err != nil {
		t.Fatal(err)
	}
	s, err := fixtureOpen(context.Background(), filepath.Join(normal, "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	pipe := filepath.Join(root, "pipe")
	if err := os.MkdirAll(pipe, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(pipe, "relay.sqlite3"), 0o600); err != nil {
		t.Fatal(err)
	}

	dirs := siblingStoreDirs(root, "")
	for _, dir := range dirs {
		if dir == pipe {
			t.Fatalf("sibling discovery offered %s, whose relay.sqlite3 is not a regular file: %v", pipe, dirs)
		}
	}
	found := false
	for _, dir := range dirs {
		if dir == normal {
			found = true
		}
	}
	if !found {
		t.Fatalf("sibling discovery did not offer the normal store: %v", dirs)
	}
}

// TestStoreFileRace_aPathBecomingAFifoBetweenStatAndOpenIsRefused pins the post-open half of the
// regular-file check: the identity stat sees a regular file, the path becomes a FIFO before the
// open, and the open (O_NONBLOCK, so it returns at once) is followed by an fstat that refuses and
// closes the descriptor.
func TestStoreFileRace_aPathBecomingAFifoBetweenStatAndOpenIsRefused(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := openDescriptorsUnder(t, dir)
	withStoreFileStatHook(t, path, func() {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := syscall.Mkfifo(path, 0o600); err != nil {
			t.Fatal(err)
		}
	})
	_, err := holdStoreFile(path)
	if err == nil {
		t.Fatal("a path that became a FIFO between the stat and the open was held instead of refused")
	}
	if !errors.Is(err, syscall.ENXIO) {
		t.Fatalf("a FIFO at a store-file path: err = %v, want ENXIO", err)
	}
	// Only this test's own descriptors are counted, and a decrease is tolerated: a finalizer that
	// closes an unrelated descriptor must not fail the check, while a leaked descriptor of this
	// test's own FIFO still does.
	if after := openDescriptorsUnder(t, dir); after > before {
		t.Fatalf("a refused FIFO left a descriptor open: %d -> %d", before, after)
	} else if after < before {
		t.Logf("the descriptor count under the test root fell from %d to %d; a finalizer closed an unrelated descriptor", before, after)
	}
}

// TestStoreFileRace_repeatedRefusalsDoNotExhaustDescriptors pins the cost of the artifact
// refusal: a path this process already knows as a store file is refused before it is opened, so
// verifying the same store-file artifact again and again does not leave a descriptor behind each
// time.
func TestStoreFileRace_repeatedRefusalsDoNotExhaustDescriptors(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()

	// The first refusal is allowed to cost one descriptor (the path is not yet known by identity
	// on the first call); every later one must not.
	if _, _, _, err := HashArtifact(context.Background(), path, []string{root}, false); err == nil {
		t.Fatal("the store file was hashed instead of refused")
	}
	before := openDescriptorsUnder(t, root)
	for range 25 {
		if _, _, _, err := HashArtifact(context.Background(), path, []string{root}, false); err == nil {
			t.Fatal("the store file was hashed instead of refused")
		}
	}
	if after := openDescriptorsUnder(t, root); after > before {
		t.Fatalf("repeated store-file refusals grew this test's descriptors: %d -> %d (CRW-880)", before, after)
	} else if after < before {
		t.Logf("the descriptor count under the test root fell from %d to %d; a finalizer closed an unrelated descriptor", before, after)
	}
}

// TestStoreFileRace_hashArtifactRefusesAStoreFileThisProcessOpened is defect 3: the artifact
// reader refuses a store file this process holds or opened, hands the descriptor to the
// registry instead of closing it, and still hashes an ordinary artifact.
func TestStoreFileRace_hashArtifactRefusesAStoreFileThisProcessOpened(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	inode := storeFileInodeOf(t, path)

	ordinary := filepath.Join(root, "out.txt")
	if err := os.WriteFile(ordinary, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	digest, size, _, err := HashArtifact(context.Background(), ordinary, []string{root}, false)
	if err != nil {
		t.Fatalf("an ordinary artifact was refused: %v", err)
	}
	if size != int64(len("payload")) || len(digest) != 64 {
		t.Fatalf("an ordinary artifact hashed to %s of %d bytes", digest, size)
	}

	before, _ := storeFileLocks(os.Getpid(), inode)
	requireObservableLock(t, inode, before, "the artifact reads")

	_, _, _, err = HashArtifact(context.Background(), path, []string{root}, false)
	requireReason(t, err, ReasonScopeEscape)
	if !strings.Contains(err.Error(), "relay store file") {
		t.Fatalf("the refusal does not name a relay store file: %v", err)
	}

	link := filepath.Join(root, "artifact-link.txt")
	if err := os.Link(path, link); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	_, _, _, err = HashArtifact(context.Background(), link, []string{root}, false)
	requireReason(t, err, ReasonScopeEscape)

	if !systemSQLitePeer(t, path) {
		t.Log("python3 is absent, so the system-SQLite-peer layer of this test did not run")
	}
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "the artifact reads")
}
