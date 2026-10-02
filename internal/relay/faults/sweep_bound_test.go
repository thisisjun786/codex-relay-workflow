package faults

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// seedSweepHistory inserts n deliveries evt-000... that the sweep has nothing to say about, but for
// the one numbered mark. mode "held" makes that one a held delivery (the delivery_stalled source reads
// it); mode "failed" gives every delivery one settled attempt and makes the marked one's attempt failed
// (the delivery_retrying source reads it).
func seedSweepHistory(t *testing.T, l *Ledger, c context.Context, n, mark int, mode string) {
	t.Helper()
	for i := 0; i < n; i++ {
		event := fmt.Sprintf("evt-%03d", i)
		state, hold := "acknowledged", any(nil)
		switch {
		case mode == "held" && i == mark:
			state, hold = "withheld_pre_send", "host_lost_turn"
		case mode == "failed":
			state = "queued"
		}
		if _, err := l.Store.Q(c).ExecContext(c, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES(?,'rel','completion','recipient','thread',?,1,?,'stamp','stamp')", event, state, hold); err != nil {
			t.Fatal(err)
		}
		if mode == "failed" {
			attempt := "dispatched"
			if i == mark {
				attempt = "transport_failed"
			}
			if _, err := l.Store.Q(c).ExecContext(c, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES(?,?,1,'completion','settled',?,'stamp')", "req-"+event, event, attempt); err != nil {
				t.Fatal(err)
			}
		}
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
			seedSweepHistory(t, l, c, 100, 90, source.mode)
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

// A source of no more rows than the window is read whole in one sweep, as it always was.
func Test_CRW293_ASourceWithinTheWindowIsReadWholeInOneSweep(t *testing.T) {
	l, c := testLedger(t)
	seedSweepHistory(t, l, c, 3, 1, "held")
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6}
	batch := sweepTick(t, sw, l, c)
	if len(batch.Observations) != 1 || cursorAt(batch, "delivery_stalled") != "" || !contains(batch.CompleteSources, "delivery_stalled") {
		t.Fatalf("observations %d, cursor %q, complete %v", len(batch.Observations), cursorAt(batch, "delivery_stalled"), batch.CompleteSources)
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
	seedSweepHistory(t, l, c, 3, -1, "held")
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO, MaxAttempts: 6}
	sweepTick(t, sw, l, c)
	before := cursorRows(t, l, c)
	if len(before) == 0 {
		t.Fatal("the first sweep recorded no cursor row")
	}
	var guards []string
	for _, verb := range []string{"INSERT", "UPDATE", "DELETE"} {
		guards = append(guards, "no_cursor_write_"+verb)
		if _, err := l.Store.Q(c).ExecContext(c, "CREATE TRIGGER no_cursor_write_"+verb+" BEFORE "+verb+" ON fault_cursors BEGIN SELECT RAISE(ABORT,'a cursor that did not move was written'); END"); err != nil {
			t.Fatal(err)
		}
	}
	clock.now += 20
	sweepTick(t, sw, l, c)
	if after := cursorRows(t, l, c); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Fatalf("a tick that moved no cursor changed the rows: %v -> %v", before, after)
	}
	for _, name := range guards {
		if _, err := l.Store.Q(c).ExecContext(c, "DROP TRIGGER "+name); err != nil {
			t.Fatal(err)
		}
	}
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
	if all := strings.Join(plan, "\n"); strings.Contains(all, "attempts_open") {
		t.Fatalf("the presence check scans settled attempts through attempts_open:\n%s", all)
	}
}
