package state

// The case of the verification of 80d8d69f (CRW-1097): a lock-time verdict that cannot be kept. The holder of the session lock must
// not change the state when the verdict on an earlier writer's pending event could not be written or the event could not be removed,
// or a later drain judges the event from a state this holder made.

import (
	"errors"
	"os"
	"testing"
)

// outboxLockOutboxReadOnly makes the session's outbox directory one nothing can be created in or removed from, and restores it when
// the test ends.
func outboxLockOutboxReadOnly(t *testing.T, cwd string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("a read-only directory does not stop root")
	}
	dir := ledgerOutboxDir(cwd, "s1")
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	return dir
}

// Red on 80d8d69f: the same-phase reset was published and its writer stopped before its drain; the next holder of the lock could not
// keep the published verdict, changed the state anyway, and the drain then read a state matching neither side and dropped the row of
// a transition that happened.
func TestLedgerOutboxLockRefusesTheWriterWhenAPublishedVerdictCannotBeKept(t *testing.T) {
	cwd := t.TempDir()
	ev := outboxSamePhasePrepared(t, cwd)
	published := ReadState(cwd, "s1")
	published.Flags.AuditPassed = false
	if err := WriteState(cwd, published); err != nil {
		t.Fatal(err)
	}
	published = ReadState(cwd, "s1")
	dir := outboxLockOutboxReadOnly(t, cwd)
	ran := false
	err := WithSessionLock(cwd, "s1", func() error {
		ran = true
		s := ReadState(cwd, "s1")
		s.Flags.CheckPassed = true
		return WriteState(cwd, s)
	})
	if err == nil || !errors.Is(err, ErrLedgerJudgment) || ran {
		t.Fatalf("lock: err=%v ran=%v", err, ran)
	}
	if now := ReadState(cwd, "s1"); stateDigest(now) != stateDigest(published) {
		t.Fatalf("the state changed under a verdict that was not kept: %+v", now.Flags)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := WithSessionLock(cwd, "s1", func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 1 || report.Dropped != 0 || report.Err != nil {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 1 || lines[0] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}

// The other verdict: an event that was never published and cannot be removed keeps the holder from writing too, so the drain still
// finds the state its writer left and drops the event.
func TestLedgerOutboxLockRefusesTheWriterWhenAnUnpublishedEventCannotBeRemoved(t *testing.T) {
	cwd := t.TempDir()
	outboxSamePhasePrepared(t, cwd)
	before := ReadState(cwd, "s1")
	dir := outboxLockOutboxReadOnly(t, cwd)
	ran := false
	err := WithSessionLock(cwd, "s1", func() error { ran = true; return nil })
	if err == nil || !errors.Is(err, ErrLedgerJudgment) || ran {
		t.Fatalf("lock: err=%v ran=%v", err, ran)
	}
	if now := ReadState(cwd, "s1"); stateDigest(now) != stateDigest(before) {
		t.Fatalf("the state changed: %+v", now.Flags)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 0 || report.Dropped != 1 || report.Err != nil {
		t.Fatalf("report: %+v", report)
	}
}

// An outbox that cannot be listed cannot be judged either, so the holder does not write.
func TestLedgerOutboxLockRefusesTheWriterWhenTheOutboxCannotBeRead(t *testing.T) {
	cwd := t.TempDir()
	outboxSamePhasePrepared(t, cwd)
	if os.Geteuid() == 0 {
		t.Skip("an unreadable directory does not stop root")
	}
	dir := ledgerOutboxDir(cwd, "s1")
	if err := os.Chmod(dir, 0o300); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	ran := false
	err := WithSessionLock(cwd, "s1", func() error { ran = true; return nil })
	if err == nil || !errors.Is(err, ErrLedgerJudgment) || ran {
		t.Fatalf("lock: err=%v ran=%v", err, ran)
	}
}

// With nothing pending the lock is what it was: no outbox, no judgement, fn runs.
func TestLedgerOutboxLockRunsTheWriterWithNothingPending(t *testing.T) {
	cwd := t.TempDir()
	ran := false
	if err := WithSessionLock(cwd, "s1", func() error { ran = true; return nil }); err != nil || !ran {
		t.Fatalf("lock: err=%v ran=%v", err, ran)
	}
}
