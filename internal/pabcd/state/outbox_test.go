package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
)

// outboxRow is a chat row of the edge from>to.
func outboxRow(sessionID string, from, to Phase) *LedgerEntry {
	return &LedgerEntry{TS: "2026-10-10T00:00:00.000Z", SessionID: sessionID, From: &from, To: to, Reason: "chat", Actor: "human"}
}

// outboxPrepared writes pre as the session's state and prepares the event of pre>post, without publishing post.
func outboxPrepared(t *testing.T, cwd string, pre State, to Phase) (LedgerEvent, State) {
	t.Helper()
	if err := WriteState(cwd, pre); err != nil {
		t.Fatal(err)
	}
	pre = ReadState(cwd, pre.SessionID)
	post := pre
	post.Phase = to
	ev, err := NewLedgerEvent(cwd, pre, post, outboxRow(pre.SessionID, pre.Phase, to), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
	return ev, post
}

func outboxLedgerLines(t *testing.T, cwd string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(cwd, crwdir.DirName, LedgerFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(string(raw))
}

// An event whose state was never published is dropped without a row, and the outbox leaves no trace.
func TestLedgerOutboxDropsAnUnpublishedTransition(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	outboxPrepared(t, cwd, pre, PhaseA)
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Dropped != 1 || report.Appended != 0 || len(report.Pending) != 0 || report.Err != nil {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 0 {
		t.Fatalf("an unpublished transition got a row: %v", lines)
	}
	if _, err := os.Stat(ledgerOutboxDir(cwd, "s1")); !os.IsNotExist(err) {
		t.Fatalf("the outbox outlived its last event: %v", err)
	}
}

// An event whose state was published gets its row once: a second drain, or a row already in the ledger
// after the recorded size, appends nothing more.
func TestLedgerOutboxAppendsAPublishedTransitionOnce(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	ev, post := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, post); err != nil {
		t.Fatal(err)
	}
	// The row is already there (an append whose removal of the event did not happen).
	if err := AppendLedgerLine(cwd, ev.Line); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Appended != 0 || report.Dropped != 0 || len(report.Pending) != 0 {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 1 || lines[0] != string(ev.Line) {
		t.Fatalf("the ledger: %v", lines)
	}
	DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if lines := outboxLedgerLines(t, cwd); len(lines) != 1 {
		t.Fatalf("a second drain appended again: %v", lines)
	}
}

// The same line before the size recorded at preparation is an earlier row, not this event's: it is
// appended again.
func TestLedgerOutboxOnlyLooksPastTheRecordedSize(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	if err := WriteState(cwd, pre); err != nil {
		t.Fatal(err)
	}
	pre = ReadState(cwd, "s1")
	post := pre
	post.Phase = PhaseA
	line, err := object(outboxRow("s1", PhaseP, PhaseA).members())
	if err != nil {
		t.Fatal(err)
	}
	if err := AppendLedgerLine(cwd, line); err != nil {
		t.Fatal(err)
	}
	ev, err := NewLedgerEvent(cwd, pre, post, outboxRow("s1", PhaseP, PhaseA), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareLedgerEvent(cwd, ev); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(cwd, post); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 1 {
		t.Fatalf("report: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 2 {
		t.Fatalf("the ledger: %v", lines)
	}
}

// An append that fails stops the drain: the later event waits, so the rows keep their order once the
// ledger works again; the failed event is marked published so it is not judged again from a state that
// moved on.
func TestLedgerOutboxKeepsTheOrderAcrossAFailedAppend(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	first, mid := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, mid); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(cwd, crwdir.DirName, LedgerFile)
	if err := os.MkdirAll(ledger, 0o755); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if len(report.Pending) != 1 || report.Err == nil {
		t.Fatalf("report: %+v", report)
	}
	second, last := outboxPrepared(t, cwd, ReadState(cwd, "s1"), PhaseB)
	if err := WriteState(cwd, last); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); len(report.Pending) != 2 {
		t.Fatalf("report with the ledger still blocked: %+v", report)
	}
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Appended != 2 || len(report.Pending) != 0 {
		t.Fatalf("report: %+v", report)
	}
	lines := outboxLedgerLines(t, cwd)
	if len(lines) != 2 || lines[0] != string(first.Line) || lines[1] != string(second.Line) {
		t.Fatalf("the ledger order: %v", lines)
	}
}

// The witness: a transition that does not move the phase (C>C keeps the phase) is judged by the state's
// digest, and a state another writer touched after a transition that was never published still reads as
// unpublished by its phase.
func TestLedgerOutboxJudgesByDigestAndPhase(t *testing.T) {
	pre := DefaultState("s1", "")
	pre.Phase, pre.UpdatedAt = PhaseC, "2026-10-10T00:00:00.000Z"
	post := pre
	epoch := "c-new"
	post.CheckEpoch = &epoch
	ev := LedgerEvent{PrePhase: PhaseC, PostPhase: PhaseC, PreDigest: stateDigest(pre), PostDigest: stateDigest(post), PreUpdatedAt: pre.UpdatedAt}
	published := post
	published.UpdatedAt = pre.UpdatedAt // the same millisecond: only the digest tells
	if !ledgerEventPublished(ev, published, stateDigest(published)) {
		t.Error("a same-phase transition published in the same millisecond read as unpublished")
	}
	if ledgerEventPublished(ev, pre, stateDigest(pre)) {
		t.Error("the untouched state read as published")
	}
	moved := DefaultState("s1", "")
	moved.Phase, moved.MemoryWriteRequested, moved.UpdatedAt = PhaseP, true, "2026-10-10T00:00:05.000Z"
	ev2 := LedgerEvent{PrePhase: PhaseP, PostPhase: PhaseA, PreDigest: "x", PostDigest: "y", PreUpdatedAt: "2026-10-10T00:00:00.000Z"}
	if ledgerEventPublished(ev2, moved, stateDigest(moved)) {
		t.Error("a state a participating writer touched read as the unpublished transition's")
	}
}

// A file that does not decode is reported and left alone, never guessed at.
func TestLedgerOutboxLeavesADamagedEventAlone(t *testing.T) {
	cwd := t.TempDir()
	if err := WriteState(cwd, DefaultState("s1", "")); err != nil {
		t.Fatal(err)
	}
	dir := ledgerOutboxDir(cwd, "s1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "00000000000000000001-ev-x.event"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if len(report.Damaged) != 1 || report.Appended != 0 {
		t.Fatalf("report: %+v", report)
	}
	if _, err := os.Stat(filepath.Join(dir, "00000000000000000001-ev-x.event")); err != nil {
		t.Fatalf("the damaged file was touched: %v", err)
	}
}

// A temp file a writer killed while staging an event left behind is removed by the next drain, and the
// outbox directory goes with it.
func TestLedgerOutboxRemovesAnOrphanTempFile(t *testing.T) {
	cwd := t.TempDir()
	if err := WriteState(cwd, DefaultState("s1", "")); err != nil {
		t.Fatal(err)
	}
	dir := ledgerOutboxDir(cwd, "s1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, "00000000000000000001-ev-x.event.123.abc.tmp")
	if err := os.WriteFile(orphan, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("the outbox with an orphan temp file was not cleaned: %v", err)
	}
}
