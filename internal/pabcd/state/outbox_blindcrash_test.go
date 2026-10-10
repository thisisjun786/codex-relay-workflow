package state

// A writer killed inside the drain, around the append of a row to a ledger the process may append to and not read (CRW-1097 with the
// write-only ledger of CRW-1103): the retry must never append the row a second time, and a row it cannot prove absent waits for a
// ledger it can search instead of being appended blind.

import (
	"os"
	"testing"
)

// errDrainKilled is the panic a test seam raises to stop the drain where a kill would.
type errDrainKilled struct{}

// drainKilledAt runs one drain whose append is replaced by appendFn, which panics as the kill; it reports whether the drain was killed.
func drainKilledAt(t *testing.T, cwd, sessionID string, appendFn func(cwd string, line []byte) error) (killed bool) {
	t.Helper()
	real := appendLedgerRow
	appendLedgerRow = appendFn
	defer func() {
		appendLedgerRow = real
		if r := recover(); r != nil {
			if _, ok := r.(errDrainKilled); !ok {
				panic(r)
			}
			killed = true
		}
	}()
	DrainLedgerOutbox(cwd, sessionID, LedgerDrainOptions{})
	return false
}

// publishedBlindEvent prepares and publishes P>A in a workspace whose ledger is write-only.
func publishedBlindEvent(t *testing.T) (cwd, path string, ev LedgerEvent) {
	t.Helper()
	cwd = t.TempDir()
	path = outboxWriteOnlyLedger(t, cwd)
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	ev, post := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, post); err != nil {
		t.Fatal(err)
	}
	return cwd, path, ev
}

// Red on 63b65d5b: the event said nothing about an append until after it, so a writer killed between the append and the event's
// rewrite left a row the retry could not see in a ledger it could not read, and the retry appended it again.
func TestLedgerOutboxWriteOnlyLedgerKilledAfterTheAppendIsNotAppendedTwice(t *testing.T) {
	cwd, path, ev := publishedBlindEvent(t)
	if !drainKilledAt(t, cwd, "s1", func(cwd string, line []byte) error {
		if err := AppendLedgerLine(cwd, line); err != nil {
			t.Fatal(err)
		}
		panic(errDrainKilled{})
	}) {
		t.Fatal("the drain was not killed")
	}
	// The retry cannot read the ledger, and the ledger grew since the append began: the row may be there, so it is not appended.
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Appended != 0 || len(report.Pending) != 1 || report.Err == nil {
		t.Fatalf("retry on the write-only ledger: %+v", report)
	}
	if lines := outboxReadableLines(t, path); len(lines) != 2 || lines[1] != string(ev.Line) {
		t.Fatalf("ledger after the retry: %v", lines)
	}
	// Once the ledger can be read, the row is found there and the event is finished without a second append.
	report = DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Appended != 0 || len(report.Pending) != 0 || report.Err != nil {
		t.Fatalf("drain on the readable ledger: %+v", report)
	}
	if lines := outboxReadableLines(t, path); len(lines) != 2 {
		t.Fatalf("ledger at the end: %v", lines)
	}
	if _, err := os.Stat(ledgerOutboxDir(cwd, "s1")); !os.IsNotExist(err) {
		t.Fatalf("the outbox outlived its last event: %v", err)
	}
}

// A writer killed after the event recorded the coming append and before the append itself left the ledger as it was: the retry sees
// the size unchanged, so the row is certainly absent, and appends it once.
func TestLedgerOutboxWriteOnlyLedgerKilledBeforeTheAppendAppendsOnce(t *testing.T) {
	cwd, path, ev := publishedBlindEvent(t)
	if !drainKilledAt(t, cwd, "s1", func(string, []byte) error { panic(errDrainKilled{}) }) {
		t.Fatal("the drain was not killed")
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Appended != 1 || len(report.Pending) != 0 || report.Err != nil {
		t.Fatalf("retry: %+v", report)
	}
	if lines := outboxReadableLines(t, path); len(lines) != 2 || lines[1] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}

// A writer killed after an append to a ledger it could read, retried while the ledger is write-only, does not append blind either.
func TestLedgerOutboxReadableAppendKilledThenWriteOnlyIsNotAppendedTwice(t *testing.T) {
	cwd, path, ev := publishedBlindEvent(t)
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if !drainKilledAt(t, cwd, "s1", func(cwd string, line []byte) error {
		if err := AppendLedgerLine(cwd, line); err != nil {
			t.Fatal(err)
		}
		panic(errDrainKilled{})
	}) {
		t.Fatal("the drain was not killed")
	}
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 0 || len(report.Pending) != 1 {
		t.Fatalf("retry on the write-only ledger: %+v", report)
	}
	if lines := outboxReadableLines(t, path); len(lines) != 2 || lines[1] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}
