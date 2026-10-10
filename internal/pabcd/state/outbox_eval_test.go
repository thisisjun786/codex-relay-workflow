package state

// The cases of the pre-merge evaluation of 3fceb240 (CRW-1097, CRW-1100, CRW-1103, CRW-1111): event order across a backward
// clock, a transition that was never published and an unrelated write, and a ledger the process may append to but not read.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// A clock that steps backwards between two events that are both still pending must not put the later event first.
func TestLedgerOutboxSequenceFollowsPreparationOrderAcrossABackwardClock(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	if err := WriteState(cwd, pre); err != nil {
		t.Fatal(err)
	}
	pre = ReadState(cwd, "s1")
	mid, last := pre, pre
	mid.Phase, last.Phase = PhaseA, PhaseB
	first, err := NewLedgerEvent(cwd, pre, mid, outboxRow("s1", PhaseP, PhaseA), nil)
	if err != nil {
		t.Fatal(err)
	}
	// The first event was stamped by a clock that is ahead of the one that now reads: an hour.
	first.Seq += 3600 * 1e9
	if err := PrepareLedgerEvent(cwd, first); err != nil {
		t.Fatal(err)
	}
	second, err := NewLedgerEvent(cwd, mid, last, outboxRow("s1", PhaseA, PhaseB), nil)
	if err != nil {
		t.Fatal(err)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("the later event has sequence %d, not above the pending one's %d", second.Seq, first.Seq)
	}
	if err := PrepareLedgerEvent(cwd, second); err != nil {
		t.Fatal(err)
	}
	pending, _, err := PendingLedgerEvents(cwd, "s1")
	if err != nil || len(pending) != 2 || pending[0].ID != first.ID || pending[1].ID != second.ID {
		t.Fatalf("pending order: %+v %v", pending, err)
	}
}

// outboxSamePhasePrepared is the event of a transition that keeps the phase and changes the state (the IDLE>IDLE reset that clears a
// leftover marker), prepared and not published.
func outboxSamePhasePrepared(t *testing.T, cwd string) LedgerEvent {
	t.Helper()
	pre := DefaultState("s1", "")
	pre.Flags.AuditPassed = true
	if err := WriteState(cwd, pre); err != nil {
		t.Fatal(err)
	}
	pre = ReadState(cwd, "s1")
	post := pre
	post.Flags.AuditPassed = false
	ev, err := NewLedgerEvent(cwd, pre, post, outboxRow("s1", PhaseIdle, PhaseIdle), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// Red on 3fceb240: a writer that stopped before it published its same-phase transition, and any other write of the session after it
// (a memory grant, a scan record), left a state that matched neither side; the changed updatedAt was taken for the publication and
// the drain appended a row for a transition that never happened. The next holder of the session lock judges first.
func TestLedgerOutboxAnUnrelatedWriteIsNotTheUnpublishedTransition(t *testing.T) {
	cwd := t.TempDir()
	outboxSamePhasePrepared(t, cwd)
	err := WithSessionLock(cwd, "s1", func() error {
		s := ReadState(cwd, "s1")
		s.Flags.CheckPassed = true
		return WriteState(cwd, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 0 || len(report.Pending) != 0 {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 0 {
		t.Fatalf("a transition that never happened got a row: %v", lines)
	}
}

// The same without the lock: a state that matches neither side of a same-phase transition is not shown to be its publication.
func TestLedgerOutboxAStateMatchingNeitherSideIsNotAPublication(t *testing.T) {
	cwd := t.TempDir()
	outboxSamePhasePrepared(t, cwd)
	s := ReadState(cwd, "s1")
	s.Flags.CheckPassed = true
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 0 || report.Dropped != 1 {
		t.Fatalf("report: %+v", report)
	}
}

// The control: a transition that was published keeps its verdict when a later writer changes the state before any drain, so its row is
// still recorded.
func TestLedgerOutboxAPublishedTransitionSurvivesALaterUnrelatedWrite(t *testing.T) {
	cwd := t.TempDir()
	ev := outboxSamePhasePrepared(t, cwd)
	s := ReadState(cwd, "s1")
	s.Flags.AuditPassed = false
	if err := WriteState(cwd, s); err != nil {
		t.Fatal(err)
	}
	err := WithSessionLock(cwd, "s1", func() error {
		s := ReadState(cwd, "s1")
		s.Flags.CheckPassed = true
		return WriteState(cwd, s)
	})
	if err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 1 || len(report.Pending) != 0 {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 1 || lines[0] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}

// outboxWriteOnlyLedger makes the workspace ledger a file the process may append to and not read.
func outboxWriteOnlyLedger(t *testing.T, cwd string) string {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("root reads a write-only file")
	}
	if _, err := crwdir.EnsureDir(cwd); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(cwd, crwdir.DirName, LedgerFile)
	if err := os.WriteFile(path, []byte("{\"earlier\":1}\n"), 0o200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })
	return path
}

func outboxReadableLines(t *testing.T, path string) []string {
	t.Helper()
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// Red on 3fceb240: the drain searched the ledger before it appended, so a ledger that may be appended to and not read left every
// applied transition's row pending for good, where the appender has always taken the row.
func TestLedgerOutboxAppendsToAWriteOnlyLedger(t *testing.T) {
	cwd := t.TempDir()
	path := outboxWriteOnlyLedger(t, cwd)
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	ev, post := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, post); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Err != nil || report.Appended != 1 || len(report.Pending) != 0 {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxReadableLines(t, path); len(lines) != 2 || lines[1] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}

// A sync that fails after the append to a write-only ledger keeps the event; the retry only syncs, so the row is in the ledger once.
func TestLedgerOutboxDoesNotAppendTwiceToAWriteOnlyLedger(t *testing.T) {
	cwd := t.TempDir()
	path := outboxWriteOnlyLedger(t, cwd)
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	ev, post := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, post); err != nil {
		t.Fatal(err)
	}
	real := syncLedgerFile
	t.Cleanup(func() { syncLedgerFile = real })
	syncLedgerFile = func(string) error { return errors.New("injected sync failure") }
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Err == nil || len(report.Pending) != 1 || report.Appended != 1 {
		t.Fatalf("first drain: %+v", report)
	}
	syncLedgerFile = real
	if err := os.Chmod(path, 0o200); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Err != nil || len(report.Pending) != 0 || report.Appended != 0 {
		t.Fatalf("retry: %+v", report)
	}
	if lines := outboxReadableLines(t, path); len(lines) != 2 || lines[1] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}
