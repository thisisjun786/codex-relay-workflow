package manage

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

// C1: a store whose last writer closed (SQLite removes the log and its index at the checkpointed
// close) is read with no sidecar created beside it.
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
	wait, err := capacityReceiptWaitFor(context.Background(), dir, "parent-1", since)
	if err != nil {
		t.Fatalf("capacityReceiptWaitFor: %v", err)
	}
	if wait.Count != 1 || wait.MedianMinutes == nil || *wait.MedianMinutes != 10 {
		t.Errorf("the wait = %+v, want the one receipt (10 minutes)", wait)
	}
	assertNoReceiptSidecar(t, path)
}

// capacityLiveWALStore builds a relay store in write-ahead mode whose one receipt is committed only
// to the log: the writer stays open (and does not checkpoint), so the log and its shared-memory
// index exist beside the store the way a running relay leaves them. The caller closes the writer.
func capacityLiveWALStore(t *testing.T, dir, event, parent string, waitMinutes float64) (string, *sql.DB) {
	t.Helper()
	path := filepath.Join(dir, relayReadStoreFile)
	capacityTestStore(t, path)
	writer, err := sql.Open("sqlite", "file:"+path)
	capacityTestMust(t, err)
	writer.SetMaxOpenConns(1)
	for _, statement := range []string{"PRAGMA journal_mode=WAL", "PRAGMA wal_autocheckpoint=0"} {
		if _, err := writer.Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	created := capacityTestNow.Add(-time.Duration(waitMinutes+1) * time.Minute)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"INSERT INTO deliveries (event_id, recipient_task_id, created_at) VALUES (?,?,?)", []any{event, parent, capacityStamp(created)}},
		{"INSERT INTO acks (event_id, ack_at) VALUES (?,?)", []any{event, capacityStamp(created.Add(time.Duration(waitMinutes * float64(time.Minute))))}},
		{"INSERT INTO events (event_id, outcome) VALUES (?,?)", []any{event, "ready_for_review"}},
	} {
		if _, err := writer.Exec(statement.query, statement.args...); err != nil {
			t.Fatal(err)
		}
	}
	return path, writer
}

// C1: a receipt committed only to a live write-ahead log (the log and its index both present, the
// store file not yet holding the commit) is read, and the read leaves the log and index as they were.
// A reader that skipped the log (immutable) or refused every non-empty log would miss it.
func TestCapacityReceiptWaitReadsAReceiptCommittedOnlyToALiveLog(t *testing.T) {
	dir := t.TempDir()
	path, writer := capacityLiveWALStore(t, dir, "e-live", "parent-1", 10)
	defer writer.Close()
	main, err := os.ReadFile(path)
	capacityTestMust(t, err)
	if bytes.Contains(main, []byte("e-live")) {
		t.Fatal("the receipt reached the store file; the test needs it only in the log")
	}
	walBefore, err := os.ReadFile(path + "-wal")
	capacityTestMust(t, err)
	if len(walBefore) <= 32 || !bytes.Contains(walBefore, []byte("e-live")) {
		t.Fatalf("the log holds no frame with the receipt (%d bytes)", len(walBefore))
	}
	shmBefore, err := os.Stat(path + "-shm")
	capacityTestMust(t, err)
	since := capacityTestNow.Add(-24 * time.Hour)
	wait, err := capacityReceiptWaitFor(context.Background(), dir, "parent-1", since)
	if err != nil {
		t.Fatalf("capacityReceiptWaitFor: %v", err)
	}
	if wait.Count != 1 || wait.MedianMinutes == nil || *wait.MedianMinutes != 10 {
		t.Errorf("the wait = %+v, want the one receipt of the log (10 minutes)", wait)
	}
	walAfter, err := os.ReadFile(path + "-wal")
	capacityTestMust(t, err)
	if !bytes.Equal(walBefore, walAfter) {
		t.Errorf("the read changed the log (%d -> %d bytes)", len(walBefore), len(walAfter))
	}
	if info, err := os.Stat(path + "-shm"); err != nil || info.Size() != shmBefore.Size() {
		t.Errorf("the read changed the shared-memory index: %v, %v", info, err)
	}
}

// C1: a genuine write-ahead log with committed frames beside no shared-memory index (the state an
// unclean shutdown leaves) can be read only by building the index, which is a sidecar, so the wait
// refuses on that ground and creates nothing.
func TestCapacityReceiptWaitRefusesWithoutCreatingASidecar(t *testing.T) {
	liveDir := t.TempDir()
	livePath, writer := capacityLiveWALStore(t, liveDir, "e-crashed", "parent-1", 10)
	defer writer.Close()
	main, err := os.ReadFile(livePath)
	capacityTestMust(t, err)
	wal, err := os.ReadFile(livePath + "-wal")
	capacityTestMust(t, err)
	// The crashed copy: the store file and a real log with frames, and no index beside them.
	dir := t.TempDir()
	path := filepath.Join(dir, relayReadStoreFile)
	capacityTestMust(t, os.WriteFile(path, main, 0o600))
	capacityTestMust(t, os.WriteFile(path+"-wal", wal, 0o600))
	since := capacityTestNow.Add(-24 * time.Hour)
	_, err = capacityReceiptWaitFor(context.Background(), dir, "parent-1", since)
	if err == nil {
		t.Fatal("capacityReceiptWaitFor read a store whose log holds frames without its index")
	}
	if !errors.Is(err, ErrRelayStoreUnreadable) || !strings.Contains(err.Error(), "write-ahead log") {
		t.Errorf("the refusal = %v, want ErrRelayStoreUnreadable naming the write-ahead log", err)
	}
	if _, err := os.Stat(path + "-shm"); !os.IsNotExist(err) {
		t.Errorf("the refused read left a shared-memory index beside the store (stat err %v)", err)
	}
	after, err := os.ReadFile(path + "-wal")
	if err != nil || !bytes.Equal(after, wal) {
		t.Errorf("the refused read changed the log: %d bytes, %v", len(after), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 2 {
		t.Errorf("the refused read created a file beside the store: %v", entries)
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
