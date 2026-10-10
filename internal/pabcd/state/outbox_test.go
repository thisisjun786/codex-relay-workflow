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

// A row counts as recorded only after the ledger is fsynced: a failed sync leaves the event pending (published, so it is not
// judged again), and the next drain finds the row, syncs, and removes the event.
func TestLedgerOutboxKeepsAnEventWhoseLedgerSyncFailed(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	ev, mid := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, mid); err != nil {
		t.Fatal(err)
	}
	real := syncLedgerFile
	t.Cleanup(func() { syncLedgerFile = real })
	syncLedgerFile = func(string) error { return errors.New("injected sync failure") }
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Err == nil || len(report.Pending) != 1 {
		t.Fatalf("a failed sync finished the event: %+v", report)
	}
	pending, _, err := PendingLedgerEvents(cwd, "s1")
	if err != nil || len(pending) != 1 || !pending[0].Published {
		t.Fatalf("the event after a failed sync: %+v %v", pending, err)
	}
	syncLedgerFile = real
	report = DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Err != nil || len(report.Pending) != 0 || report.Appended != 0 {
		t.Fatalf("the retry: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 1 || lines[0] != string(ev.Line) {
		t.Fatalf("ledger: %v", lines)
	}
}

// A sync that reaches both the ledger file and its directory.
func TestLedgerOutboxSyncsTheFileAndItsDirectory(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	_, mid := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, mid); err != nil {
		t.Fatal(err)
	}
	var synced []string
	real := syncLedgerFile
	t.Cleanup(func() { syncLedgerFile = real })
	syncLedgerFile = func(path string) error { synced = append(synced, filepath.Base(path)); return real(path) }
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Err != nil || report.Appended != 1 {
		t.Fatalf("report: %+v", report)
	}
	if len(synced) != 2 || synced[0] != LedgerFile || synced[1] != crwdir.DirName {
		t.Fatalf("synced: %v", synced)
	}
}

// Two distinct events whose rows are byte for byte the same (same millisecond, same edge) are two rows: the second does not take
// the first's row for its own, in one drain or across a drain that stopped between them.
func TestLedgerOutboxRecordsDistinctEventsWithIdenticalRows(t *testing.T) {
	for _, blocked := range []bool{false, true} {
		cwd := t.TempDir()
		pre := DefaultState("s1", "")
		pre.Phase = PhaseP
		if err := WriteState(cwd, pre); err != nil {
			t.Fatal(err)
		}
		pre = ReadState(cwd, "s1")
		mid := pre
		mid.Phase = PhaseA
		ledger := filepath.Join(cwd, crwdir.DirName, LedgerFile)
		if blocked {
			if err := os.MkdirAll(ledger, 0o755); err != nil {
				t.Fatal(err)
			}
		}
		var evs [2]LedgerEvent
		for i := range evs {
			ev, err := NewLedgerEvent(cwd, pre, mid, outboxRow("s1", PhaseP, PhaseA), nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := PrepareLedgerEvent(cwd, ev); err != nil {
				t.Fatal(err)
			}
			evs[i] = ev
		}
		if evs[0].ID == evs[1].ID || string(evs[0].Line) != string(evs[1].Line) || evs[0].LedgerOffset != evs[1].LedgerOffset {
			t.Fatalf("the two events are not identical but for their ids: %+v %+v", evs[0], evs[1])
		}
		if err := WriteState(cwd, mid); err != nil {
			t.Fatal(err)
		}
		if blocked {
			if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Err == nil || len(report.Pending) != 2 {
				t.Fatalf("blocked report: %+v", report)
			}
			if err := os.Remove(ledger); err != nil {
				t.Fatal(err)
			}
		}
		report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
		if report.Appended != 2 || len(report.Pending) != 0 || report.Err != nil {
			t.Fatalf("blocked=%v report: %+v", blocked, report)
		}
		if lines := outboxLedgerLines(t, cwd); len(lines) != 2 {
			t.Fatalf("blocked=%v ledger: %v", blocked, lines)
		}
	}
}

// The end of a recorded row is carried to the next identical event: when the drain stops at the second one (its ledger sync
// fails), the retry still records the second row once and does not take the first row for it.
func TestLedgerOutboxCarriesTheRowEndToTheNextIdenticalEvent(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	if err := WriteState(cwd, pre); err != nil {
		t.Fatal(err)
	}
	pre = ReadState(cwd, "s1")
	mid := pre
	mid.Phase = PhaseA
	for range 2 {
		ev, err := NewLedgerEvent(cwd, pre, mid, outboxRow("s1", PhaseP, PhaseA), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := PrepareLedgerEvent(cwd, ev); err != nil {
			t.Fatal(err)
		}
	}
	if err := WriteState(cwd, mid); err != nil {
		t.Fatal(err)
	}
	real := syncLedgerFile
	t.Cleanup(func() { syncLedgerFile = real })
	calls := 0
	syncLedgerFile = func(path string) error {
		if calls++; calls == 3 {
			return errors.New("injected sync failure")
		}
		return real(path)
	}
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Err == nil || len(report.Pending) != 1 {
		t.Fatalf("first drain: %+v", report)
	}
	syncLedgerFile = real
	if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{}); report.Err != nil || len(report.Pending) != 0 {
		t.Fatalf("retry: %+v", report)
	}
	if lines := outboxLedgerLines(t, cwd); len(lines) != 2 {
		t.Fatalf("ledger: %v", lines)
	}
}

// When a followup that is still pending is followed by an event whose append fails, the report keeps both events, in order.
func TestLedgerOutboxPendingReportKeepsBothEvents(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	first, mid := outboxPrepared(t, cwd, pre, PhaseA)
	first.Followup = []byte(`{"x":1}`)
	if err := writeLedgerEvent(cwd, first); err != nil {
		t.Fatal(err)
	}
	if err := WriteState(cwd, mid); err != nil {
		t.Fatal(err)
	}
	second, last := outboxPrepared(t, cwd, ReadState(cwd, "s1"), PhaseB)
	if err := WriteState(cwd, last); err != nil {
		t.Fatal(err)
	}
	real := syncLedgerFile
	t.Cleanup(func() { syncLedgerFile = real })
	calls := 0
	syncLedgerFile = func(path string) error {
		if calls++; calls == 3 {
			return errors.New("injected sync failure")
		}
		return real(path)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{Followup: func(LedgerEvent) error { return errors.New("followup failed") }})
	if len(report.Pending) != 2 || report.Pending[0].ID != first.ID || report.Pending[1].ID != second.ID {
		t.Fatalf("pending: %+v", report.Pending)
	}
}

// Verification round 2: three transitions, each published and drained while the ledger cannot be appended to, the second of which
// returns the session to the phase the first one left. A drain that stops at the first blocked append still records that its
// caller published its own event, so once the ledger works again every row is appended, in order, and none is judged again from a
// state that has moved on (red at d9a805df: the round trip's middle event was dropped).
func TestLedgerOutboxKeepsEveryPublishedEventAcrossABlockedLedger(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	if err := WriteState(cwd, pre); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(cwd, crwdir.DirName, LedgerFile)
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, to := range []Phase{PhaseA, PhaseP, PhaseA} {
		cur := ReadState(cwd, "s1")
		next := cur
		next.Phase = to
		ev, err := NewLedgerEvent(cwd, cur, next, outboxRow("s1", cur.Phase, to), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := PrepareLedgerEvent(cwd, ev); err != nil {
			t.Fatal(err)
		}
		if err := WriteState(cwd, next); err != nil {
			t.Fatal(err)
		}
		if report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{Published: map[string]bool{ev.ID: true}}); report.Err == nil || report.Dropped != 0 {
			t.Fatalf("the blocked drain: %+v", report)
		}
	}
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Appended != 3 || report.Dropped != 0 || len(report.Pending) != 0 || report.Err != nil {
		t.Fatalf("a published transition was lost: %+v", report)
	}
	want := []string{`"from":"P","to":"A"`, `"from":"A","to":"P"`, `"from":"P","to":"A"`}
	lines := outboxLedgerLines(t, cwd)
	if len(lines) != len(want) {
		t.Fatalf("ledger: %v", lines)
	}
	for i, w := range want {
		if !strings.Contains(lines[i], w) {
			t.Fatalf("row %d is %s, want %s", i, lines[i], w)
		}
	}
}

// A drain stopped by a blocked append still judges the events after the blocked one against the state found now: an event whose
// writer stopped before its publication is dropped, the published one is kept as published.
func TestLedgerOutboxJudgesTheEventsBehindABlockedAppend(t *testing.T) {
	cwd := t.TempDir()
	pre := DefaultState("s1", "")
	pre.Phase = PhaseP
	first, mid := outboxPrepared(t, cwd, pre, PhaseA)
	if err := WriteState(cwd, mid); err != nil {
		t.Fatal(err)
	}
	ledger := filepath.Join(cwd, crwdir.DirName, LedgerFile)
	if err := os.Mkdir(ledger, 0o700); err != nil {
		t.Fatal(err)
	}
	// A second writer prepared A>B and stopped before it published it.
	cur := ReadState(cwd, "s1")
	next := cur
	next.Phase = PhaseB
	second, err := NewLedgerEvent(cwd, cur, next, outboxRow("s1", PhaseA, PhaseB), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := PrepareLedgerEvent(cwd, second); err != nil {
		t.Fatal(err)
	}
	report := DrainLedgerOutbox(cwd, "s1", LedgerDrainOptions{})
	if report.Err == nil || report.Dropped != 1 || len(report.Pending) != 1 || report.Pending[0].ID != first.ID || !report.Pending[0].Published {
		t.Fatalf("the blocked drain: %+v", report)
	}
	pending, _, _ := PendingLedgerEvents(cwd, "s1")
	if len(pending) != 1 || pending[0].ID != first.ID || !pending[0].Published {
		t.Fatalf("pending: %+v", pending)
	}
}
