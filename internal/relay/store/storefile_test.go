package store

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// CRW-846: the registry holds one descriptor per (device, inode) for the life of the process.

func TestStoreFileRegistry_returnsTheSameHandleForOneFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := holdStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := holdStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("the registry opened a second descriptor for one path: %p != %p", first, second)
	}
}

func TestStoreFileRegistry_reusesTheHandleForASecondNameOfOneInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.sqlite3")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	first, err := holdStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := holdStoreFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("a second name for one inode got its own descriptor: %p != %p", first, second)
	}
}

// TestStoreFileRegistry_doesNotOpenASecondDescriptorForAKnownInode is the review finding this
// change fixed: a second name for an inode the process already holds must not open a descriptor
// that nothing keeps, because os.File's finalizer would close it at the next collection and drop
// this process's POSIX locks on the file.
func TestStoreFileRegistry_doesNotOpenASecondDescriptorForAKnownInode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "relay.sqlite3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(dir, "alias.sqlite3")
	if err := os.Link(path, alias); err != nil {
		t.Skipf("hard links unavailable here: %v", err)
	}
	if _, err := holdStoreFile(path); err != nil {
		t.Fatal(err)
	}
	before := openDescriptorCount(t)
	second, err := holdStoreFile(alias)
	if err != nil {
		t.Fatal(err)
	}
	if after := openDescriptorCount(t); after != before {
		t.Fatalf("a second name for a held inode opened a descriptor that nothing keeps: %d -> %d", before, after)
	}
	// The handle the second name got is the held one, and it is still usable.
	if _, err := second.Stat(); err != nil {
		t.Fatalf("the reused handle is not usable: %v", err)
	}
}

// TestStoreFileRegistry_refusesANonRegularFile pins the review's second finding: a directory (or
// any non-regular file) at a store-file path is refused with the EISDIR refusal CopySnapshot
// documents, and no descriptor is held for it.
func TestStoreFileRegistry_refusesANonRegularFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	before := openDescriptorCount(t)
	_, err := holdStoreFile(dir)
	if !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("a directory at a store-file path: err = %v, want EISDIR", err)
	}
	if after := openDescriptorCount(t); after != before {
		t.Fatalf("a refused non-regular file left a descriptor open: %d -> %d", before, after)
	}
}

// openDescriptorCount is the number of descriptors this process holds.
func openDescriptorCount(t *testing.T) int {
	t.Helper()
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Skipf("/proc/self/fd is unavailable: %v", err)
	}
	return len(entries)
}

func TestStoreFileRegistry_doesNotGrowWithRepeatedReads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := holdStoreFile(path); err != nil {
		t.Fatal(err)
	}
	before := heldStoreFileCount()
	for range 25 {
		if _, err := holdStoreFile(path); err != nil {
			t.Fatal(err)
		}
	}
	if after := heldStoreFileCount(); after != before {
		t.Fatalf("the held set grew over repeats: %d -> %d", before, after)
	}
}

func TestStoreFileRegistry_reportsAnAbsentFile(t *testing.T) {
	if _, err := holdStoreFile(filepath.Join(t.TempDir(), "relay.sqlite3")); !os.IsNotExist(err) {
		t.Fatalf("an absent store file: err = %v, want not-exist", err)
	}
}

// TestStoreFileRegistry_neverClosesWhatItHolds is the rule itself: no exported or package
// function of this package closes a handle the registry returned.
func TestStoreFileRegistry_neverClosesWhatItHolds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.sqlite3")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := holdStoreFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// A second read of the same path must still work: the handle is open.
	if _, err := holdStoreFile(path); err != nil {
		t.Fatalf("the held handle was closed: %v", err)
	}
	if _, err := file.Stat(); err != nil {
		t.Fatalf("the held handle is not usable: %v", err)
	}
}
