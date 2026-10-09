package manage

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"database/sql"

	_ "modernc.org/sqlite"
)

// CRW-1043: the receipt wait reads the relay store the way the relay's own reads resolve the path
// (a symbolic link is followed before a ".." is applied), and its read-only open creates no SQLite
// sidecar. Both properties already hold on this branch (relayReadOpenStore, store.InPlaceRead); these
// tests pin them on the receipt wait itself.

// C1: a state spelling through a link and ".." reads the store the kernel names, not the store a
// cleaned spelling would name. <root>/link points at <root>/real/inner, so <root>/link/../state is
// <root>/real/state; the decoy at <root>/state holds a different wait, which a cleaned path would read.
func TestCapacityReceiptWaitResolvesTheStateSpellingThroughALink(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "real", "inner"), 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(root, "real", "state")
	decoy := filepath.Join(root, "state")
	for _, dir := range []string{real, decoy} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	capacityTestStore(t, filepath.Join(real, relayReadStoreFile))
	capacityTestReceipt(t, &capacityFixture{relayDir: real}, "e-real", "parent-1", 10, false)
	capacityTestStore(t, filepath.Join(decoy, relayReadStoreFile))
	capacityTestReceipt(t, &capacityFixture{relayDir: decoy}, "e-decoy", "parent-1", 500, false)
	if err := os.Symlink(filepath.Join(root, "real", "inner"), filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	// Concatenated, so no Join or Clean touches the spelling before the read does.
	spelling := root + "/link/../state"
	since := capacityTestNow.Add(-24 * time.Hour)
	wait, err := capacityReceiptWaitFor(context.Background(), spelling, "parent-1", since)
	if err != nil {
		t.Fatalf("capacityReceiptWaitFor(%q): %v", spelling, err)
	}
	if wait.Count != 1 || wait.MedianMinutes == nil || *wait.MedianMinutes != 10 {
		t.Errorf("the wait = %+v, want the one receipt of the real store (10 minutes), not the decoy's 500", wait)
	}
}

// C1: a read-only open of a store with no write-ahead log creates no sidecar, and neither does a
// read of a store whose log is present with its index.
func TestCapacityReceiptWaitCreatesNoSidecarBesideACleanStore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, relayReadStoreFile)
	db, err := sql.Open("sqlite", "file:"+path)
	capacityTestMust(t, err)
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		t.Fatal(err)
	}
	capacityTestMust(t, db.Close())
	capacityTestStore(t, path)
	capacityTestReceipt(t, &capacityFixture{relayDir: dir}, "e1", "parent-1", 10, false)
	assertNoReceiptSidecar(t, path)
	since := capacityTestNow.Add(-24 * time.Hour)
	if _, err := capacityReceiptWaitFor(context.Background(), dir, "parent-1", since); err != nil {
		t.Fatalf("capacityReceiptWaitFor: %v", err)
	}
	assertNoReceiptSidecar(t, path)
}

// C1: a write-ahead log that holds frames beside no shared-memory index can be read only by building
// the index, which is a sidecar, so the wait refuses and creates nothing.
func TestCapacityReceiptWaitRefusesWithoutCreatingASidecar(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, relayReadStoreFile)
	capacityTestStore(t, path)
	capacityTestReceipt(t, &capacityFixture{relayDir: dir}, "e1", "parent-1", 10, false)
	// A log past its header (32 bytes) with no index beside it: the frames are not readable without one.
	wal := path + "-wal"
	capacityTestMust(t, os.WriteFile(wal, make([]byte, 64), 0o600))
	since := capacityTestNow.Add(-24 * time.Hour)
	if _, err := capacityReceiptWaitFor(context.Background(), dir, "parent-1", since); err == nil {
		t.Fatal("capacityReceiptWaitFor read a store whose log holds frames without its index")
	}
	if _, err := os.Stat(path + "-shm"); !os.IsNotExist(err) {
		t.Errorf("the refused read left a shared-memory index beside the store (stat err %v)", err)
	}
	if info, err := os.Stat(wal); err != nil || info.Size() != 64 {
		t.Errorf("the refused read changed the log: %v, %v", info, err)
	}
}

func assertNoReceiptSidecar(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); !os.IsNotExist(err) {
			t.Errorf("a read left %s beside the store (stat err %v)", suffix, err)
		}
	}
}
