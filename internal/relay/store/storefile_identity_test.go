//go:build linux

package store

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CRW-967: the store-file identity table. A store holds a reference on its database file while it is
// open (store.open, the read-only openers and the projection copy), and the artifact reader, the
// authorized openers and the independent-review reader refuse a file that table holds. The tests
// below use only temporary state and never touch the operational relay store.

// identityTableSize is the number of entries the identity table holds: one per open store identity
// and one per open store path. A closed store must leave it as it found it.
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

func TestStoreFileIdentity_repeatedOpenAndCloseLeavesTheTableAsItWas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seed, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
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
	seed, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
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
	seed, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
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
	other := filepath.Join(dir, "unrelated.txt")
	if err := os.WriteFile(other, []byte("unrelated artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	if identityKeyOf(t, other).inode != key.inode {
		t.Logf("the kernel did not reuse inode %d for the unrelated artifact; the table check above is the assertion", key.inode)
		return
	}
	if _, _, _, err := HashArtifact(ctx, other, []string{dir}, false); err != nil {
		t.Fatalf("an unrelated artifact on a reused inode was refused: %v", err)
	}
}

func TestStoreFileIdentity_readOnlyOpenersFillAndReleaseTheTable(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	seed, err := fixtureOpen(ctx, path, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
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

	// A symbolic link to the store file is followed by the open, so only the opened descriptor's
	// identity shows it: the refusal and the descriptor's handover must still hold.
	link := filepath.Join(dir, "review-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	_, err = ReadIndependentFile(link, 1<<20)
	requireReason(t, err, ReasonScopeEscape)
	judgeStoreFileLockAfter(t, os.Getpid(), inode, before, "the refused independent review reads")
}

func TestStoreFileIdentity_authorizedHandleKeepsAStoreFileOpenedAfterIt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "plain.sqlite3")
	if err := os.WriteFile(path, []byte("not yet a store"), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := OpenAuthorized(path, []string{dir}, false)
	if err != nil {
		t.Fatal(err)
	}
	fd := int(h.File.Fd())
	live := registerLiveStore(path)
	live.attach()
	defer live.release()

	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if !descriptorIsOpen(fd) {
		t.Fatal("closing an artifact handle closed a descriptor of a store file opened after it was taken")
	}
}

// TestStoreFileIdentity_aStoreOpenedDuringTheReadIsRecognisedBeforeTheClose is the interleaving test
// for the gap between the identity check and the close: a store takes its reference on the file
// after the read's checks and before the read closes its descriptor. The close must keep the
// descriptor, decided under the registry lock.
func TestStoreFileIdentity_aStoreOpenedDuringTheReadIsRecognisedBeforeTheClose(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path := filepath.Join(dir, "artifact.bin")
	if err := os.WriteFile(path, []byte("artifact bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	var fdSeen = -1
	var live *liveStoreRef
	saved := betweenPasses
	betweenPasses = func(fd int, _ *statSnapshot) {
		fdSeen = fd
		live = registerLiveStore(path)
		live.attach()
	}
	t.Cleanup(func() { betweenPasses = saved })
	_, _, _, err := HashArtifact(ctx, path, []string{dir}, false)
	if err != nil {
		t.Fatalf("the read of an ordinary artifact failed: %v", err)
	}
	defer live.release()
	if fdSeen < 0 {
		t.Fatal("the read did not reach the seam between its passes")
	}
	if !descriptorIsOpen(fdSeen) {
		t.Fatal("the read closed a descriptor of a file a store took a reference on during the read")
	}
}

// TestStoreFileIdentity_independentReadKeepsADescriptorOpenedDuringTheRead is the same gap for
// delivery's independent-review reader: the store takes the reference between the check and the close.
func TestStoreFileIdentity_independentReadKeepsADescriptorOpenedDuringTheRead(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "review.json")
	if err := os.WriteFile(path, []byte(`{"verdict":"ok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var seen *os.File
	var live *liveStoreRef
	raw, err := readIndependentFile(path, 1<<20, func(file *os.File) {
		seen = file
		live = registerLiveStore(path)
		live.attach()
	})
	defer live.release()
	if err != nil {
		t.Fatalf("the independent review read failed: %v", err)
	}
	if string(raw) != `{"verdict":"ok"}` {
		t.Fatalf("the read returned %q", raw)
	}
	if seen == nil || !descriptorIsOpen(int(seen.Fd())) {
		t.Fatal("the read closed a descriptor of a file a store took a reference on during the read")
	}
}
