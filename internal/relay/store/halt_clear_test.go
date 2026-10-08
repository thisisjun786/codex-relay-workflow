package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store/ownership"
)

// The store-halt-clear tests (CRW-885). Every store is a temporary one; the marker is written by
// RecordHalt, the same publisher a detection uses, and cleared by ClearHalt.

// haltClearFixture is a store with a detection's marker beside it and the store closed, as the operator
// finds it. It returns the store's path.
func haltClearFixture(t *testing.T) string {
	t.Helper()
	s, dir := haltFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "relay.sqlite3")
	if err := RecordHalt(context.Background(), path, CorruptingCause{Code: 522, Message: "disk I/O error", Site: HaltSiteObservation}); err != nil {
		t.Fatal(err)
	}
	return path
}

// haltClearReadingFile writes one reading the operator names and returns its path and sha256.
func haltClearReadingFile(t *testing.T, name, text string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(text))
	return path, hex.EncodeToString(sum[:])
}

// haltClearRows reads the journal's store_halt_cleared rows back through a read-only open, which works
// whether or not the marker is present.
func haltClearRows(t *testing.T, path string) []string {
	t.Helper()
	s, err := Open(WithReadOnlyCommand(context.Background()), path, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close() }()
	rows, err := s.DB.Query("SELECT detail FROM journal WHERE kind = 'store_halt_cleared' ORDER BY seq")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var details []string
	for rows.Next() {
		var detail string
		if err := rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

func TestHaltClearRefusesAMissingReadingAndKeepsTheMarker(t *testing.T) {
	path := haltClearFixture(t)
	restore, _ := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	_, err := ClearHalt(context.Background(), path, HaltClearInput{
		RestorePath: restore, ReconcilePath: filepath.Join(t.TempDir(), "absent.txt"), Actor: "op", Reason: "restored",
	})
	if RefusalReason(err) != ReasonMalformedReceipt {
		t.Fatalf("a missing reading was not refused malformed_receipt: %v", err)
	}
	if !HaltStateAt(path).Present {
		t.Fatal("the marker was removed although a reading was missing")
	}
	if rows := haltClearRows(t, path); len(rows) != 0 {
		t.Fatalf("a refused clear wrote %d journal rows", len(rows))
	}
}

func TestHaltClearRefusesAnEmptyReadingAndKeepsTheMarker(t *testing.T) {
	path := haltClearFixture(t)
	restore, _ := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	empty, _ := haltClearReadingFile(t, "reconcile.txt", "")
	_, err := ClearHalt(context.Background(), path, HaltClearInput{
		RestorePath: restore, ReconcilePath: empty, Actor: "op", Reason: "restored",
	})
	if RefusalReason(err) != ReasonMalformedReceipt {
		t.Fatalf("an empty reading was not refused malformed_receipt: %v", err)
	}
	if !HaltStateAt(path).Present {
		t.Fatal("the marker was removed although a reading was empty")
	}
}

func TestHaltClearWithBothReadingsRemovesTheMarkerAndWritesOneRow(t *testing.T) {
	path := haltClearFixture(t)
	before := HaltStateAt(path).Marker
	restore, restoreSum := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	reconcile, reconcileSum := haltClearReadingFile(t, "reconcile.txt", "reconcile reading\n")
	result, err := ClearHalt(context.Background(), path, HaltClearInput{
		RestorePath: restore, ReconcilePath: reconcile, Actor: "operator-1", Reason: "restore and reconcile agree",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Cleared {
		t.Fatal("a marker with both readings was not cleared")
	}
	if HaltStateAt(path).Present {
		t.Fatal("the marker is still present after the clear")
	}
	rows := haltClearRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("the clear wrote %d journal rows, want one", len(rows))
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(rows[0]), &detail); err != nil {
		t.Fatalf("the journal row is not JSON: %v: %s", err, rows[0])
	}
	if detail["markerSequence"] != float64(before.Sequence) || detail["markerDetectedAt"] != before.DetectedAt {
		t.Fatalf("the row does not carry the marker's sequence and time: %v", detail)
	}
	if detail["restoreSha256"] != restoreSum || detail["reconcileSha256"] != reconcileSum {
		t.Fatalf("the row does not carry both reading digests: %v", detail)
	}
	if detail["reason"] != "restore and reconcile agree" {
		t.Fatalf("the row does not carry the reason: %v", detail)
	}
	// A writable store opens again afterwards.
	s, err := Open(context.Background(), path, "")
	if err != nil {
		t.Fatalf("the writable store did not open after the clear: %v", err)
	}
	defer func() { _ = s.Close() }()
	if err := writeJournal(t, s); err != nil {
		t.Fatalf("a write after the clear was refused: %v", err)
	}
}

func TestHaltClearWithoutAMarkerChangesNothing(t *testing.T) {
	s, dir := haltFixture(t)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "relay.sqlite3")
	restore, _ := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	reconcile, _ := haltClearReadingFile(t, "reconcile.txt", "reconcile reading\n")
	result, err := ClearHalt(context.Background(), path, HaltClearInput{
		RestorePath: restore, ReconcilePath: reconcile, Actor: "op", Reason: "nothing to clear",
	})
	if err != nil {
		t.Fatalf("a clear with no marker was refused: %v", err)
	}
	if result.Cleared {
		t.Fatal("a clear with no marker reported that it cleared one")
	}
	if rows := haltClearRows(t, path); len(rows) != 0 {
		t.Fatalf("a clear with no marker wrote %d journal rows", len(rows))
	}
}

func TestHaltClearNeedsTheWriteGateExclusively(t *testing.T) {
	path := haltClearFixture(t)
	gate, err := ownership.Lock(filepath.Join(filepath.Dir(path), "write-gate.lock"), false, false)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = gate.Close() }()
	restore, _ := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	reconcile, _ := haltClearReadingFile(t, "reconcile.txt", "reconcile reading\n")
	_, err = ClearHalt(context.Background(), path, HaltClearInput{
		RestorePath: restore, ReconcilePath: reconcile, Actor: "op", Reason: "gate held",
	})
	if RefusalReason(err) != "store_owned_by_other" {
		t.Fatalf("a clear beside another writer was not refused store_owned_by_other: %v", err)
	}
	if !HaltStateAt(path).Present {
		t.Fatal("the marker was removed while another writer held the gate")
	}
}

func TestHaltClearRecordsAHostileReasonAsOneJSONRow(t *testing.T) {
	path := haltClearFixture(t)
	restore, _ := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	reconcile, _ := haltClearReadingFile(t, "reconcile.txt", "reconcile reading\n")
	reason := "quote \" backslash \\ line\nbreak \u2028 " + strings.Repeat("x", 300)
	if _, err := ClearHalt(context.Background(), path, HaltClearInput{
		RestorePath: restore, ReconcilePath: reconcile, Actor: "op", Reason: reason,
	}); err != nil {
		t.Fatal(err)
	}
	rows := haltClearRows(t, path)
	if len(rows) != 1 {
		t.Fatalf("want one row, got %d", len(rows))
	}
	var detail map[string]any
	if err := json.Unmarshal([]byte(rows[0]), &detail); err != nil {
		t.Fatalf("the row is not JSON: %v", err)
	}
	if detail["reason"] != reason {
		t.Fatal("the reason did not round-trip through the row")
	}
}

// haltClearFault makes the named clear point fail with err for the rest of the test.
func haltClearFault(t *testing.T, point string, err error) {
	t.Helper()
	SetHaltFault(func(p string) error {
		if p == point {
			return err
		}
		return nil
	})
	t.Cleanup(func() { SetHaltFault(nil) })
}

// haltClearInputs writes the two readings a clear names, returning their paths.
func haltClearInputs(t *testing.T, reconcileText string) HaltClearInput {
	t.Helper()
	restore, _ := haltClearReadingFile(t, "restore.txt", "restore reading\n")
	reconcile, _ := haltClearReadingFile(t, "reconcile.txt", reconcileText)
	return HaltClearInput{RestorePath: restore, ReconcilePath: reconcile, Actor: "operator-1", Reason: "restore and reconcile agree"}
}

func TestHaltClearAbsentStoreWithNoMarkerCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	result, err := ClearHalt(context.Background(), filepath.Join(dir, "relay.sqlite3"), haltClearInputs(t, "reconcile reading\n"))
	if err != nil || result.Cleared {
		t.Fatalf("an absent store with no marker answered %v, cleared %v", err, result.Cleared)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.Name() != "restore.txt" && entry.Name() != "reconcile.txt" {
			t.Fatalf("the clear created %s in an empty state directory", entry.Name())
		}
	}
}

func TestHaltClearFailureBeforeTheRowKeepsTheMarkerAndWritesNoRow(t *testing.T) {
	path := haltClearFixture(t)
	haltClearFault(t, "clear-before-row", errors.New("injected before the row"))
	if _, err := ClearHalt(context.Background(), path, haltClearInputs(t, "reconcile reading\n")); err == nil {
		t.Fatal("a clear that failed before its row reported success")
	}
	if !HaltStateAt(path).Present {
		t.Fatal("the marker was removed although the row was never written")
	}
	if rows := haltClearRows(t, path); len(rows) != 0 {
		t.Fatalf("a failed clear wrote %d rows", len(rows))
	}
}

func TestHaltClearRetryAfterAFailedRemovalWritesOneRow(t *testing.T) {
	path := haltClearFixture(t)
	input := haltClearInputs(t, "reconcile reading\n")
	haltClearFault(t, "clear-row-committed", errors.New("injected after the row"))
	if _, err := ClearHalt(context.Background(), path, input); err == nil {
		t.Fatal("a clear that failed after its row reported success")
	}
	if !HaltStateAt(path).Present {
		t.Fatal("the marker was removed although the clear failed before the removal")
	}
	if rows := haltClearRows(t, path); len(rows) != 1 {
		t.Fatalf("the failed clear left %d rows, want one", len(rows))
	}
	SetHaltFault(nil)
	result, err := ClearHalt(context.Background(), path, input)
	if err != nil || !result.Cleared {
		t.Fatalf("the retry did not clear the marker: %v, cleared %v", err, result.Cleared)
	}
	if HaltStateAt(path).Present {
		t.Fatal("the marker is still present after the retry")
	}
	if rows := haltClearRows(t, path); len(rows) != 1 {
		t.Fatalf("the retry wrote a second row: %d rows", len(rows))
	}
}

func TestHaltClearRetryWithOtherReadingsIsRefused(t *testing.T) {
	path := haltClearFixture(t)
	haltClearFault(t, "clear-row-committed", errors.New("injected after the row"))
	if _, err := ClearHalt(context.Background(), path, haltClearInputs(t, "reconcile reading\n")); err == nil {
		t.Fatal("a clear that failed after its row reported success")
	}
	SetHaltFault(nil)
	if _, err := ClearHalt(context.Background(), path, haltClearInputs(t, "other reconcile reading\n")); err == nil {
		t.Fatal("a retry with other readings was not refused")
	}
	if !HaltStateAt(path).Present {
		t.Fatal("the marker was removed by a refused retry")
	}
	if rows := haltClearRows(t, path); len(rows) != 1 {
		t.Fatalf("the refused retry changed the rows: %d", len(rows))
	}
}

func TestHaltClearFollowsALinkedStateDirectory(t *testing.T) {
	path := haltClearFixture(t)
	linked := filepath.Join(t.TempDir(), "linked")
	if err := os.Symlink(filepath.Dir(path), linked); err != nil {
		t.Fatal(err)
	}
	result, err := ClearHalt(context.Background(), filepath.Join(linked, "relay.sqlite3"), haltClearInputs(t, "reconcile reading\n"))
	if err != nil || !result.Cleared {
		t.Fatalf("a clear through a linked state directory did not clear: %v, cleared %v", err, result.Cleared)
	}
	if HaltStateAt(path).Present {
		t.Fatal("the marker beside the resolved store is still present")
	}
	if rows := haltClearRows(t, path); len(rows) != 1 {
		t.Fatalf("the clear through the link wrote %d rows beside the resolved store", len(rows))
	}
}
