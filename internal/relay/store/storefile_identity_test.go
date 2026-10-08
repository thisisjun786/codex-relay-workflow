//go:build linux

package store

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CRW-967: the store-file identity table. A store holds a reference on its database file while it is
// open (store.open, the read-only openers and the projection copy). The artifact reader, the authorized
// opener and the independent-review reader refuse a file the table recognises, and a descriptor a
// close keeps stays open. The tests use only temporary state and never touch the operational store.

// identityTableSize is the number of entries the identity table holds: one per open store identity and
// one per open store path. A closed store must leave it as it found it.
func identityTableSize() int {
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	return len(heldStoreFiles.live) + len(heldStoreFiles.livePaths)
}

// identityKeyOf is the (device, inode) identity of an existing file.
func identityKeyOf(t *testing.T, path string) storeFileKey {
	t.Helper()
	key, ok := storeFileKeyOf(path)
	if !ok {
		t.Fatalf("%s is not a regular file", path)
	}
	return key
}

// holdsIdentityOf reports whether the identity table recognises the file at path as an open store's
// database or sidecar. The handles the registry keeps for the process's life are not part of it.
func holdsIdentityOf(t *testing.T, path string) bool {
	t.Helper()
	key := identityKeyOf(t, path)
	heldStoreFiles.Lock()
	defer heldStoreFiles.Unlock()
	return holdsLiveStoreIdentityLocked(key.device, key.inode)
}

// descriptorIsOpen reports whether fd is still an open descriptor of this process.
func descriptorIsOpen(fd int) bool {
	_, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	return err == nil
}

// seedDatabase creates a store at path and closes it, so the file exists and no store holds it.
func seedDatabase(t *testing.T, path string) {
	t.Helper()
	seed, err := fixtureOpen(context.Background(), path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStoreFileIdentity_repeatedOpenAndCloseLeavesTheTableAsItWas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seedDatabase(t, path)
	before := identityTableSize()
	for i := 0; i < 25; i++ {
		s, err := fixtureOpen(ctx, path, "")
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		if err := s.Close(); err != nil {
			t.Fatalf("close %d: %v", i, err)
		}
	}
	if after := identityTableSize(); after != before {
		t.Fatalf("25 open and close cycles left the identity table at %d entries, want %d: a closed store keeps its entry", after, before)
	}
}

func TestStoreFileIdentity_concurrentOpenAndCloseLeavesTheTableAsItWas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seedDatabase(t, path)
	before := identityTableSize()
	var wg sync.WaitGroup
	errs := make(chan error, 24)
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				s, err := fixtureOpen(ctx, path, "")
				if err != nil {
					errs <- err
					return
				}
				if err := s.Close(); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent open or close: %v", err)
	}
	if after := identityTableSize(); after != before {
		t.Fatalf("concurrent open and close cycles left the identity table at %d entries, want %d", after, before)
	}
}

func TestStoreFileIdentity_twoStoresOnOneFileKeepTheOtherRecognised(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seedDatabase(t, path)
	first, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	second, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	if !holdsIdentityOf(t, path) {
		t.Fatal("closing one of two stores on the file dropped its identity while the other is still open")
	}
	_, _, _, err = HashArtifact(ctx, path, []string{dir}, false)
	requireReason(t, err, ReasonScopeEscape)
	if err := second.Close(); err != nil {
		t.Fatal(err)
	}
	if holdsIdentityOf(t, path) {
		t.Fatal("the last store on the file closed, and its identity is still recognised")
	}
}

func TestStoreFileIdentity_renamedDatabaseIsRecognisedUnderItsNewName(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	inode := storeFileInodeOf(t, path)
	before, _ := storeFileLocks(os.Getpid(), inode)
	requireObservableLock(t, inode, before, "the rename")

	renamed := filepath.Join(dir, "moved.sqlite3")
	if err := os.Rename(path, renamed); err != nil {
		t.Fatal(err)
	}
	_, _, _, err := HashArtifact(ctx, renamed, []string{dir}, false)
	requireReason(t, err, ReasonScopeEscape)
	var rows int
	if err := s.Querier(ctx).QueryRowContext(ctx, "SELECT count(*) FROM store_challenge").Scan(&rows); err != nil {
		t.Fatalf("the open store stopped answering after its file was renamed: %v", err)
	}
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "the renamed database was read as an artifact")
}

// TestStoreFileIdentity_aNewFileAtTheOldNameOfARenamedDatabaseIsNotRefused: the database is recognised
// by its identity, so an unrelated file that takes the name it had is an ordinary artifact.
func TestStoreFileIdentity_aNewFileAtTheOldNameOfARenamedDatabaseIsNotRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	if err := os.Rename(path, filepath.Join(dir, "moved.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("unrelated artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := HashArtifact(ctx, path, []string{dir}, false); err != nil {
		t.Fatalf("an unrelated file at the old name of a renamed database was refused: %v", err)
	}
}

func TestStoreFileIdentity_inodeOfAClosedStoreIsNotRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	key := identityKeyOf(t, path)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	heldStoreFiles.Lock()
	tableHolds := holdsLiveStoreIdentityLocked(key.device, key.inode)
	heldStoreFiles.Unlock()
	if tableHolds {
		t.Fatalf("the closed store's identity (inode %d) is still in the identity table after its database was removed", key.inode)
	}
	// The freed inode is reused by the next new file on most filesystems. Each new file is read through
	// the public reader; the one that takes the freed inode must not be refused.
	reused := false
	for i := 0; i < 256 && !reused; i++ {
		candidate := filepath.Join(dir, fmt.Sprintf("unrelated-%03d.txt", i))
		if err := os.WriteFile(candidate, []byte("unrelated artifact"), 0o600); err != nil {
			t.Fatal(err)
		}
		if identityKeyOf(t, candidate).inode != key.inode {
			continue
		}
		reused = true
		if _, _, _, err := HashArtifact(ctx, candidate, []string{dir}, false); err != nil {
			t.Fatalf("an unrelated artifact on the freed inode %d was refused: %v", key.inode, err)
		}
	}
	if !reused {
		t.Skipf("the filesystem did not reuse inode %d for 256 new files; the table check above is the assertion that held", key.inode)
	}
}

func TestStoreFileIdentity_readOnlyOpenersFillAndReleaseTheTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seedDatabase(t, path)
	before := identityTableSize()

	ro, err := OpenReadOnlyStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if !holdsIdentityOf(t, path) {
		t.Fatal("OpenReadOnlyStore did not record its database file")
	}
	if err := ro.Close(); err != nil {
		t.Fatal(err)
	}

	rr, err := OpenReadOnly(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !holdsIdentityOf(t, path) {
		t.Fatal("OpenReadOnly did not record its database file")
	}
	if err := rr.Close(); err != nil {
		t.Fatal(err)
	}

	ip, err := OpenInPlace(ctx, path, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !holdsIdentityOf(t, path) {
		t.Fatal("OpenInPlace did not record its database file")
	}
	if err := ip.Close(); err != nil {
		t.Fatal(err)
	}
	if after := identityTableSize(); after != before {
		t.Fatalf("the read-only openers left the identity table at %d entries, want %d", after, before)
	}
}

func TestStoreFileIdentity_openAuthorizedRefusesAKnownStoreFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	inode := storeFileInodeOf(t, path)
	before, _ := storeFileLocks(os.Getpid(), inode)
	requireObservableLock(t, inode, before, "OpenAuthorized")

	h, err := OpenAuthorized(path, []string{dir}, false)
	if err == nil {
		_ = h.Close()
		t.Fatal("OpenAuthorized opened a relay store file this process has open")
	}
	requireReason(t, err, ReasonScopeEscape)
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "the refused OpenAuthorized")
}

func TestStoreFileIdentity_readIndependentFileRefusesAKnownStoreFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	inode := storeFileInodeOf(t, path)
	before, _ := storeFileLocks(os.Getpid(), inode)
	requireObservableLock(t, inode, before, "the independent review read")

	_, err := ReadIndependentFile(path, 1<<20)
	requireReason(t, err, ReasonScopeEscape)

	link := filepath.Join(dir, "review-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err = ReadIndependentFile(link, 1<<20)
	requireReason(t, err, ReasonScopeEscape)
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "the refused independent review reads")
}

// TestStoreFileIdentity_repeatedReadsOfALinkToAnOpenStoreOpenNoDescriptor: a link to an open store is
// refused before it is opened, so asking again opens no descriptor and keeps none.
func TestStoreFileIdentity_repeatedReadsOfALinkToAnOpenStoreOpenNoDescriptor(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	s := openRaceStore(t, path)
	defer func() { _ = s.Close() }()
	link := filepath.Join(dir, "review-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	before := openDescriptorsUnder(t, dir)
	for i := 0; i < 50; i++ {
		_, err := ReadIndependentFile(link, 1<<20)
		requireReason(t, err, ReasonScopeEscape)
	}
	if after := openDescriptorsUnder(t, dir); after != before {
		t.Fatalf("50 refused reads of a link to an open store changed this process's descriptors under the test root from %d to %d", before, after)
	}
}

// TestStoreFileIdentity_attachRefusesAPathRenamedAfterItsReference: the identity is taken when the
// reference is taken, and a path that names another file (or nothing) when the connection opened it
// cannot record the store's identity, so the open is an error.
func TestStoreFileIdentity_attachRefusesAPathRenamedAfterItsReference(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	if err := os.WriteFile(path, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	live := registerLiveStore(path)
	defer live.release()
	if err := os.Rename(path, filepath.Join(dir, "moved.sqlite3")); err != nil {
		t.Fatal(err)
	}
	if err := live.attach(); err == nil {
		t.Fatal("attach accepted a path that no longer names the file its reference was taken on")
	}
}

func TestStoreFileIdentity_attachRefusesAPathThatNamesNothing(t *testing.T) {
	live := registerLiveStore(filepath.Join(t.TempDir(), "absent.sqlite3"))
	defer live.release()
	if err := live.attach(); err == nil {
		t.Fatal("attach accepted a path that names no file")
	}
}

// TestStoreFileIdentity_authorizedHandleKeepsADescriptorAStoreOpenedAfterIt: the handle is taken on a
// file no store holds; a store opens the file afterwards; closing the handle keeps the descriptor and
// the store still answers.
func TestStoreFileIdentity_authorizedHandleKeepsADescriptorAStoreOpenedAfterIt(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seedDatabase(t, path)
	h, err := OpenAuthorized(path, []string{dir}, false)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(h.File.Fd())
	ro, err := OpenReadOnlyStore(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !descriptorIsOpen(fd) {
		t.Fatal("closing an artifact handle closed a descriptor of a store file opened after it was taken")
	}
	var one int
	if err := ro.DB.QueryRowContext(ctx, "SELECT count(*) FROM store_challenge").Scan(&one); err != nil {
		t.Fatalf("the store that opened the file stopped answering: %v", err)
	}
}

// TestStoreFileIdentity_aStoreOpenedDuringTheReadIsRecognisedBeforeTheClose is the interleaving test
// for the gap between the identity check and the close: a store takes its reference on the database
// between the read's checks and the close of its descriptor. The close must keep the descriptor.
func TestStoreFileIdentity_aStoreOpenedDuringTheReadIsRecognisedBeforeTheClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seedDatabase(t, path)
	var fdSeen = -1
	var opened *Store
	var openErr error
	saved := betweenPasses
	betweenPasses = func(fd int, _ *statSnapshot) {
		fdSeen = fd
		opened, openErr = OpenReadOnlyStore(ctx, path)
	}
	t.Cleanup(func() {
		betweenPasses = saved
		if opened != nil {
			_ = opened.Close()
		}
	})
	_, _, _, err := HashArtifact(ctx, path, []string{dir}, false)
	if openErr != nil {
		t.Fatalf("the store opened during the read failed: %v", openErr)
	}
	if fdSeen < 0 {
		t.Fatal("the read did not reach the seam between its passes")
	}
	if err != nil {
		t.Fatalf("the read of the database failed: %v", err)
	}
	if !descriptorIsOpen(fdSeen) {
		t.Fatal("the read closed a descriptor of a database a store opened during the read")
	}
	var one int
	if err := opened.DB.QueryRowContext(ctx, "SELECT count(*) FROM store_challenge").Scan(&one); err != nil {
		t.Fatalf("the store opened during the read stopped answering: %v", err)
	}
}

// TestStoreFileIdentity_independentReadKeepsADescriptorOpenedDuringTheRead is the same gap for
// delivery's independent-review reader.
func TestStoreFileIdentity_independentReadKeepsADescriptorOpenedDuringTheRead(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	seedDatabase(t, path)
	var seen *os.File
	var opened *Store
	var openErr error
	raw, err := readIndependentFile(path, 1<<20, func(file *os.File) {
		seen = file
		opened, openErr = OpenReadOnlyStore(ctx, path)
	})
	t.Cleanup(func() {
		if opened != nil {
			_ = opened.Close()
		}
	})
	if openErr != nil {
		t.Fatalf("the store opened during the read failed: %v", openErr)
	}
	if err != nil {
		t.Fatalf("the independent review read failed: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("the read returned no bytes")
	}
	if seen == nil || !descriptorIsOpen(int(seen.Fd())) {
		t.Fatal("the read closed a descriptor of a database a store opened during the read")
	}
	var one int
	if err := opened.DB.QueryRowContext(ctx, "SELECT count(*) FROM store_challenge").Scan(&one); err != nil {
		t.Fatalf("the store opened during the read stopped answering: %v", err)
	}
}

// TestStoreFileIdentity_independentReadRefusesAFifoWithoutBlocking: a path that is not a regular file is
// refused after a non-blocking open, so a FIFO with no writer cannot hold the reader.
func TestStoreFileIdentity_independentReadRefusesAFifoWithoutBlocking(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "review.fifo")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo is unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := ReadIndependentFile(fifo, 1<<20)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a FIFO was read as an independent review")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reading a FIFO blocked: the open must not wait for a writer")
	}
}
