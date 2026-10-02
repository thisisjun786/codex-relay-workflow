package faults

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"testing"
)

// seedSweepHistory inserts n deliveries evt-000... that the sweep has nothing to say about, but for
// the marked ones. mode "held" makes a marked delivery a held one (the delivery_stalled source reads
// it); mode "failed" gives every delivery one settled attempt and makes a marked one's attempt failed
// (the delivery_retrying source reads it).
func seedSweepHistory(t *testing.T, l *Ledger, c context.Context, n int, marked func(int) bool, mode string) {
	t.Helper()
	err := l.Store.Transaction(c, func(ctx context.Context, _ *sql.Conn) error {
		for i := 0; i < n; i++ {
			event := fmt.Sprintf("evt-%03d", i)
			state, hold := "acknowledged", any(nil)
			switch {
			case mode == "held" && marked(i):
				state, hold = "withheld_pre_send", "host_lost_turn"
			case mode == "failed":
				state = "queued"
			}
			if _, err := l.Store.Q(ctx).ExecContext(ctx, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES(?,'rel','completion','recipient','thread',?,1,?,'stamp','stamp')", event, state, hold); err != nil {
				return err
			}
			if mode == "failed" {
				attempt := "dispatched"
				if marked(i) {
					attempt = "transport_failed"
				}
				if _, err := l.Store.Q(ctx).ExecContext(ctx, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES(?,?,1,'completion','settled',?,'stamp')", "req-"+event, event, attempt); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// sweepTick is one daemon tick: the sweep, then recording what it found and moving the cursors.
func sweepTick(t *testing.T, sw *Sweeper, l *Ledger, c context.Context) Batch {
	t.Helper()
	batch, err := sw.Sweep(c, "crw")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = sw.RecordAll(c, l, batch); err != nil {
		t.Fatalf("recording the tick: %v", err)
	}
	return batch
}

// sweepMove is a tick that moves the cursors without recording what the sweep found.
func sweepMove(t *testing.T, sw *Sweeper, c context.Context) Batch {
	t.Helper()
	batch, err := sw.Sweep(c, "crw")
	if err != nil {
		t.Fatal(err)
	}
	if err = sw.writeCursors(c, batch.positions, batch.stored); err != nil {
		t.Fatal(err)
	}
	return batch
}

// cursorAt is where a source's rotation stands after a sweep: "" for a rotation that ended.
func cursorAt(batch Batch, source string) string {
	position, ok := batch.Cursors[source].(map[string]any)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%v..%v", position["at"], position["until"])
}

// A source whose rotation needs more rows than one sweep may scan is read in steps: each tick scans at
// most ScanWindow of its rows, the cursor stays where the scan stopped, and the rotation ends, and the
// next one begins at the first key, only when a scan reaches the key the rotation was bounded by.
func Test_CRW293_ScanWindowStepsARotationThroughASource(t *testing.T) {
	for _, source := range []struct {
		name, mode string
		keys       []string // the cursor each of the first four ticks leaves
	}{
		{"deliveries", "held", []string{"evt-039..evt-099", "evt-079..evt-099", "", "evt-039..evt-099"}},
		{"attempts", "failed", []string{"40..100", "80..100", "", "40..100"}},
	} {
		t.Run(source.name, func(t *testing.T) {
			l, c := testLedger(t)
			seedSweepHistory(t, l, c, 100, func(i int) bool { return i == 90 }, source.mode)
			sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6, ScanWindow: 40}
			cursor := map[string]string{"deliveries": "delivery_stalled", "attempts": "delivery_retrying"}[source.name]
			var reported []int
			for tick := 1; tick <= 6; tick++ {
				batch := sweepTick(t, sw, l, c)
				if len(batch.Observations) > 0 {
					reported = append(reported, tick)
				}
				if tick <= len(source.keys) {
					if got := cursorAt(batch, cursor); got != source.keys[tick-1] {
						t.Fatalf("tick %d left cursor %q, want %q", tick, got, source.keys[tick-1])
					}
				}
				if tick == 1 && contains(batch.CompleteSources, cursor) {
					t.Fatal("a sweep that stopped inside its rotation called the source complete")
				}
			}
			// The marked row is the 91st of 100 and a window holds 40: it is out of reach of ticks 1 and 2, read
			// by tick 3, which also reaches the end of the rotation, and read again by the next rotation.
			if fmt.Sprint(reported) != "[3 6]" {
				t.Fatalf("the marked row was reported on ticks %v, want [3 6]", reported)
			}
		})
	}
}

// A source of no more rows than the window is paged as it always was: one sweep reads it whole when
// fewer than a page of its rows match.
func Test_CRW293_ASourceWithinTheWindowIsReadWholeInOneSweep(t *testing.T) {
	l, c := testLedger(t)
	seedSweepHistory(t, l, c, 3, func(i int) bool { return i == 1 }, "held")
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6}
	batch := sweepTick(t, sw, l, c)
	if len(batch.Observations) != 1 || cursorAt(batch, "delivery_stalled") != "" || !contains(batch.CompleteSources, "delivery_stalled") {
		t.Fatalf("observations %d, cursor %q, complete %v", len(batch.Observations), cursorAt(batch, "delivery_stalled"), batch.CompleteSources)
	}
}

// Whatever the window, a rotation reads every row that was there when it started exactly once, and it
// takes a tick for every sweepLimit rows that match (as it always did), a tick for every window of
// rows scanned, and the one that reaches the end.
func Test_CRW293_ARotationReadsEveryRowOnceWhateverTheWindow(t *testing.T) {
	const rows = 100
	marked := func(i int) bool { return i%3 == 0 || i > 60 }
	for _, source := range []struct{ name, mode, cursor, key string }{
		{"deliveries", "held", "delivery_stalled", "delivery:evt-%03d"},
		{"attempts", "failed", "delivery_retrying", "delivery:req-evt-%03d"},
	} {
		t.Run(source.name, func(t *testing.T) {
			l, c := testLedger(t)
			seedSweepHistory(t, l, c, rows, marked, source.mode)
			want, matches := map[string]int{}, 0
			for i := 0; i < rows; i++ {
				if marked(i) {
					want[fmt.Sprintf(source.key, i)] = 1
					matches++
				}
			}
			for _, window := range []int{1, 7, 40, 99, 100, 101, scanWindow} {
				if _, err := l.Store.Q(c).ExecContext(c, "DELETE FROM fault_cursors"); err != nil {
					t.Fatal(err)
				}
				sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6, ScanWindow: window}
				got, ticks := map[string]int{}, 0
				for ended := false; !ended; {
					if ticks++; ticks > 1000 {
						t.Fatalf("window %d: the rotation did not end", window)
					}
					batch := sweepMove(t, sw, c)
					for _, o := range batch.Observations {
						got[o.OccurrenceKey]++
					}
					ended = cursorAt(batch, source.cursor) == ""
				}
				if fmt.Sprint(got) != fmt.Sprint(want) {
					t.Fatalf("window %d: the rotation read %v, want each of %v once", window, got, want)
				}
				if limit := matches/sweepLimit + rows/window + 1; ticks > limit {
					t.Fatalf("window %d: the rotation took %d ticks, more than %d", window, ticks, limit)
				}
			}
		})
	}
}

// A source with no more settled attempts than a window is read whole in one sweep even when attempts
// that are not settled follow the last of them: the rotation's bound is the highest attempt, and the
// scan has nothing left to read before it.
func Test_CRW293_SettledAttemptsOfOneWindowAreReadWholeBesideAnAttemptInFlight(t *testing.T) {
	l, c := testLedger(t)
	seedSweepHistory(t, l, c, 5, func(int) bool { return false }, "failed")
	if _, err := l.Store.Q(c).ExecContext(c, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('req-open','evt-000',2,'completion','in_flight',NULL,'stamp')"); err != nil {
		t.Fatal(err)
	}
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6, ScanWindow: 5}
	batch := sweepTick(t, sw, l, c)
	if got := cursorAt(batch, "delivery_retrying"); got != "" || !contains(batch.CompleteSources, "delivery_retrying") {
		t.Fatalf("cursor %q, complete %v: the rotation did not end in one sweep", got, batch.CompleteSources)
	}
}

// guardCursorWrites makes every write to fault_cursors fail until the returned function lifts it.
func guardCursorWrites(t *testing.T, l *Ledger, c context.Context) (lift func()) {
	t.Helper()
	var names []string
	for _, verb := range []string{"INSERT", "UPDATE", "DELETE"} {
		names = append(names, "no_cursor_write_"+verb)
		if _, err := l.Store.Q(c).ExecContext(c, "CREATE TRIGGER no_cursor_write_"+verb+" BEFORE "+verb+" ON fault_cursors BEGIN SELECT RAISE(ABORT,'a cursor that did not move was written'); END"); err != nil {
			t.Fatal(err)
		}
	}
	return func() {
		for _, name := range names {
			if _, err := l.Store.Q(c).ExecContext(c, "DROP TRIGGER "+name); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// A position that is stored and that the sweep did not move is left alone, as one that is not stored is.
func Test_CRW293_AStoredPositionTheSweepDidNotMoveIsNotRewritten(t *testing.T) {
	l, c := testLedger(t)
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO}
	position := map[string]any{"delivery_stalled": map[string]any{"at": "evt-031", "until": "evt-099"}}
	if err := sw.writeCursors(c, position, nil); err != nil {
		t.Fatal(err)
	}
	stored, err := sw.readCursors(c)
	if err != nil {
		t.Fatal(err)
	}
	lift := guardCursorWrites(t, l, c)
	defer lift()
	if err = sw.writeCursors(c, position, stored); err != nil {
		t.Fatalf("a stored position the sweep did not move was written: %v", err)
	}
	moved := map[string]any{"delivery_stalled": map[string]any{"at": "evt-063", "until": "evt-099"}}
	if err = sw.writeCursors(c, moved, stored); err == nil {
		t.Fatal("a position the sweep moved was not written")
	}
}

func cursorRows(t *testing.T, l *Ledger, c context.Context) map[string]string {
	t.Helper()
	rows, err := l.Store.All(c, "SELECT source, position, updated_at FROM fault_cursors")
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, r := range rows {
		out[text(r, "source")] = fmt.Sprintf("%v @ %s", r.Get("position"), text(r, "updated_at"))
	}
	return out
}

// A cursor is written when its position moved, and a tick that moves none writes nothing: not a row, and
// not the transaction that would have held the store's writer lock for it.
func Test_CRW293_ACursorIsWrittenOnlyWhenItsPositionMoves(t *testing.T) {
	l, c := testLedger(t)
	clock := l.Clock.(*testClock)
	seedSweepHistory(t, l, c, 3, func(int) bool { return false }, "held")
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6}
	sweepTick(t, sw, l, c)
	before := cursorRows(t, l, c)
	if len(before) == 0 {
		t.Fatal("the first sweep recorded no cursor row")
	}
	guards := guardCursorWrites(t, l, c)
	clock.now += 20
	sweepTick(t, sw, l, c)
	if after := cursorRows(t, l, c); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a tick that moved no cursor changed the rows: %v -> %v", before, after)
	}
	guards()
	// A page that fills moves the delivery_stalled cursor, and only that row is written, with the new time.
	for i := 0; i < sweepLimit+8; i++ {
		if _, err := l.Store.Q(c).ExecContext(c, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES(?,'rel','completion','recipient','thread','withheld_pre_send',1,'host_lost_turn','stamp','stamp')", fmt.Sprintf("evt-%03d", 100+i)); err != nil {
			t.Fatal(err)
		}
	}
	clock.now += 20
	sweepTick(t, sw, l, c)
	after := cursorRows(t, l, c)
	var changed []string
	for source, row := range after {
		if row != before[source] {
			changed = append(changed, source)
		}
	}
	if fmt.Sprint(changed) != "[delivery_stalled]" || !strings.HasSuffix(after["delivery_stalled"], clock.ISO()) {
		t.Fatalf("rows changed: %v; delivery_stalled is %q, want a write stamped %s", changed, after["delivery_stalled"], clock.ISO())
	}
}

// The presence check must reach a delivery's attempts through the (event_id, attempt_no) key. The
// attempts_open index answers an equality on internal_state, which holds for almost every attempt, so a
// plan that uses it reads every settled attempt for every delivery it is asked about.
func Test_CRW293_StillPresentDoesNotScanSettledAttemptsPerDelivery(t *testing.T) {
	l, c := testLedger(t)
	args := append(append(pick("recipient"), settledDelivery...), busyCap, "transport_failed", "transport_failed", "", sweepLimit)
	rows, err := l.Store.All(c, "EXPLAIN QUERY PLAN "+stillPresentSQL, args...)
	if err != nil {
		t.Fatal(err)
	}
	var plan []string
	for _, r := range rows {
		plan = append(plan, text(r, "detail"))
	}
	if len(plan) == 0 {
		t.Fatal("no query plan")
	}
	all := strings.Join(plan, "\n")
	if strings.Contains(all, "attempts_open") {
		t.Fatalf("the presence check scans settled attempts through attempts_open:\n%s", all)
	}
	if !strings.Contains(all, "SEARCH a2 USING INDEX sqlite_autoindex_attempts_2") {
		t.Fatalf("the presence check does not reach a delivery's attempts through their (event, attempt number) key:\n%s", all)
	}
}
