package cli_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// The store-halt-clear command's tests (CRW-885): a temporary halted store, and readings the test
// writes itself.

// storeHaltClearReadings writes the two readings the operator names, returning their paths.
func storeHaltClearReadings(t *testing.T, restoreText, reconcileText string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	restore := filepath.Join(dir, "restore.txt")
	reconcile := filepath.Join(dir, "reconcile.txt")
	if err := os.WriteFile(restore, []byte(restoreText), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reconcile, []byte(reconcileText), 0o600); err != nil {
		t.Fatal(err)
	}
	return restore, reconcile
}

func TestStoreHaltClearRefusesAMissingReadingAsMalformedReceipt(t *testing.T) {
	state := haltedState(t, "clear-missing", `{"detectedAt":"2026-10-06T13:44:00.000000+00:00","pid":1,"command":"crw relay","code":522,"message":"disk I/O error","site":"observation","sequence":1}`)
	restore, _ := storeHaltClearReadings(t, "restore\n", "")
	got := goCLI(t, "--state", state, "store-halt-clear", "--restore-reading", restore, "--reconcile-reading", filepath.Join(t.TempDir(), "absent"), "--actor", "op", "--reason", "restored")
	if got.code != 2 || object(t, got.stdout)["reason"] != "malformed_receipt" {
		t.Fatalf("a missing reading was not refused malformed_receipt: %d %s", got.code, got.stdout)
	}
	if _, err := os.Stat(filepath.Join(state, "corruption.json")); err != nil {
		t.Fatalf("the marker was removed by a refused clear: %v", err)
	}
}

func TestStoreHaltClearWithBothReadingsClearsTheMarker(t *testing.T) {
	state := haltedState(t, "clear-both", `{"detectedAt":"2026-10-06T13:44:00.000000+00:00","pid":1,"command":"crw relay","code":522,"message":"disk I/O error","site":"observation","sequence":1}`)
	restore, reconcile := storeHaltClearReadings(t, "restore reading\n", "reconcile reading\n")
	got := goCLI(t, "--state", state, "store-halt-clear", "--restore-reading", restore, "--reconcile-reading", reconcile, "--actor", "op", "--reason", "restored")
	if got.code != 0 {
		t.Fatalf("the clear exited %d: %s", got.code, got.stdout)
	}
	decoded := object(t, got.stdout)
	if decoded["cleared"] != true {
		t.Fatalf("the clear did not say it cleared the marker: %s", got.stdout)
	}
	if _, err := os.Stat(filepath.Join(state, "corruption.json")); !os.IsNotExist(err) {
		t.Fatalf("the marker is still present: %v", err)
	}
	// A writable command opens again afterwards.
	if after := goCLI(t, "--state", state, "store-challenge", "--write"); after.code != 0 {
		t.Fatalf("a writable command was still refused after the clear: %d %s", after.code, after.stdout)
	}
}

func TestStoreHaltClearWithoutAMarkerSaysSo(t *testing.T) {
	home := tempHome(t)
	state := ownerOnlyState(t, home, "clear-none")
	testsupport.Create(t, filepath.Join(state, "relay.sqlite3"), "", "go")
	restore, reconcile := storeHaltClearReadings(t, "restore reading\n", "reconcile reading\n")
	got := goCLI(t, "--state", state, "store-halt-clear", "--restore-reading", restore, "--reconcile-reading", reconcile, "--actor", "op", "--reason", "nothing")
	if got.code != 0 {
		t.Fatalf("a clear with no marker exited %d: %s", got.code, got.stdout)
	}
	if object(t, got.stdout)["cleared"] != false {
		t.Fatalf("a clear with no marker did not say it cleared nothing: %s", got.stdout)
	}
}

// TestStoreHaltClearRefusesANonUTF8ReasonBeforeTouchingTheMarker: the journal row is JSON, which would
// record invalid UTF-8 as U+FFFD, so the command refuses such a value as usage and leaves the marker.
func TestStoreHaltClearRefusesANonUTF8ReasonBeforeTouchingTheMarker(t *testing.T) {
	state := haltedState(t, "clear-utf8", `{"detectedAt":"2026-10-06T13:44:00.000000+00:00","pid":1,"command":"crw relay","code":522,"message":"disk I/O error","site":"observation","sequence":1}`)
	restore, reconcile := storeHaltClearReadings(t, "restore reading\n", "reconcile reading\n")
	got := goCLI(t, "--state", state, "store-halt-clear", "--restore-reading", restore, "--reconcile-reading", reconcile, "--actor", "op", "--reason", "bad \xff bytes")
	if got.code != 4 {
		t.Fatalf("a non-UTF-8 reason exited %d, want 4: %s", got.code, got.stdout)
	}
	if _, err := os.Stat(filepath.Join(state, "corruption.json")); err != nil {
		t.Fatalf("the marker was removed for a refused reason: %v", err)
	}
}
