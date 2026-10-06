package store

import (
	"os"
	"path/filepath"
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
