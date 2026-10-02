package faults

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The fault sweep's benchmarks run over a synthetic history shaped like a long-lived store: most
// deliveries are finished (acknowledged) and carry two settled attempts, so the sweep has nothing
// to report and every tick is the cost of reading history to find that out. Run them with
//
//	go test ./internal/relay/faults -run '^$' -bench FaultSweep -benchtime 5x
const (
	historyDeliveries = 20000
	historyRecipients = 20
)

// syntheticHistory opens a store holding deliveries finished deliveries over historyRecipients
// recipients, two settled attempts each (so twice as many settled attempts as deliveries), and,
// when stalled is set, one held delivery of recipient-0 that sorts after all of them: the one
// a presence check for that recipient can only find at the end of its scan.
func syntheticHistory(b *testing.B, deliveries int, stalled bool) (*Ledger, *Sweeper) {
	b.Helper()
	ctx := context.Background()
	s, err := store.Open(ctx, filepath.Join(b.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = s.Close() })
	numbers := "WITH RECURSIVE n(i) AS (SELECT 0 UNION ALL SELECT i+1 FROM n WHERE i < ?) "
	err = s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		for _, statement := range []string{
			numbers + "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) " +
				"SELECT printf('evt-%07d', i), 'rel-' || (i % 400), 'completion', 'recipient-' || (i % " + fmt.Sprint(historyRecipients) + "), 'thread', 'acknowledged', 2, NULL, 'stamp', 'stamp' FROM n",
			numbers + "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) " +
				"SELECT printf('req-%07d-%d', i, k), printf('evt-%07d', i), k, 'completion', 'settled', 'dispatched', 'stamp' FROM n, (SELECT 1 AS k UNION ALL SELECT 2)",
		} {
			if _, err := s.Q(ctx).ExecContext(ctx, statement, deliveries-1); err != nil {
				return err
			}
		}
		if stalled {
			if _, err := s.Q(ctx).ExecContext(ctx, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES('evt-9999999','rel-0','completion','recipient-0','thread','withheld_pre_send',1,'host_lost_turn','stamp','stamp')"); err != nil {
				return err
			}
			if _, err := s.Q(ctx).ExecContext(ctx, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES('req-9999999-1','evt-9999999',1,'completion','settled','transport_failed','stamp')"); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		b.Fatal(err)
	}
	l := &Ledger{Store: s, Clock: &testClock{now: 100000}}
	return l, &Sweeper{Store: s, Now: l.Clock.ISO, MaxAttempts: 6}
}

// BenchmarkFaultSweepStillPresent is the presence check the sweep makes for every open
// delivery_stalled fault on every tick: absent is a fault whose delivery is gone, so the check
// reads every candidate; late finds its delivery only after reading all the others.
func BenchmarkFaultSweepStillPresent(b *testing.B) {
	signature := map[string]any{"recipient": "recipient-0", "attemptState": "transport_failed"}
	for _, deliveries := range []int{5000, 10000, historyDeliveries} {
		for _, stalled := range []bool{false, true} {
			name := fmt.Sprintf("deliveries=%d/%s", deliveries, map[bool]string{false: "absent", true: "late"}[stalled])
			b.Run(name, func(b *testing.B) {
				_, sw := syntheticHistory(b, deliveries, stalled)
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					present, undetermined, err := sw.stillPresent(context.Background(), signature)
					if err != nil || present != stalled || undetermined {
						b.Fatalf("present %v undetermined %v err %v", present, undetermined, err)
					}
				}
			})
		}
	}
}

// BenchmarkFaultSweepTick is one daemon tick of the sweep over a history with nothing to
// report: the reads that find every source's page short, then the cursors written back. The
// first tick of a store creates its cursor rows and is left out of the measurement. The clock
// moves 20 s a tick, as the daemon's does, so a cursor row written again is a changed row: written
// with the same bytes, SQLite leaves the page alone and the write costs nothing.
func BenchmarkFaultSweepTick(b *testing.B) {
	for _, deliveries := range []int{5000, 10000, historyDeliveries} {
		b.Run(fmt.Sprintf("deliveries=%d", deliveries), func(b *testing.B) {
			l, sw := syntheticHistory(b, deliveries, false)
			ctx := context.Background()
			clock := l.Clock.(*testClock)
			tick := func() {
				clock.now += 20
				batch, err := sw.Sweep(ctx, "crw")
				if err != nil {
					b.Fatal(err)
				}
				if len(batch.Observations)+len(batch.Clears) != 0 {
					b.Fatalf("a quiet history reported %d observations", len(batch.Observations))
				}
				if _, err = sw.RecordAll(ctx, l, batch); err != nil {
					b.Fatal(err)
				}
			}
			tick()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tick()
			}
		})
	}
}

// BenchmarkFaultSweepReads is the same tick without recording: the reads alone. No cursor moves, so
// each sweep is the first of a rotation.
func BenchmarkFaultSweepReads(b *testing.B) {
	for _, deliveries := range []int{5000, 10000, historyDeliveries} {
		b.Run(fmt.Sprintf("deliveries=%d", deliveries), func(b *testing.B) {
			_, sw := syntheticHistory(b, deliveries, false)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := sw.Sweep(context.Background(), "crw"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkFaultSweepCursorWrite is the recording step of a tick that moved no cursor: the batch is
// swept after the cursor rows exist, as every later tick's is, and holds the positions they stand at.
// The clock moves 20 s a tick, as the daemon's does (see BenchmarkFaultSweepTick).
func BenchmarkFaultSweepCursorWrite(b *testing.B) {
	l, sw := syntheticHistory(b, 1, false)
	ctx := context.Background()
	first, err := sw.Sweep(ctx, "crw")
	if err != nil {
		b.Fatal(err)
	}
	if _, err = sw.RecordAll(ctx, l, first); err != nil {
		b.Fatal(err)
	}
	batch, err := sw.Sweep(ctx, "crw")
	if err != nil {
		b.Fatal(err)
	}
	clock := l.Clock.(*testClock)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		clock.now += 20
		if _, err := sw.RecordAll(ctx, l, batch); err != nil {
			b.Fatal(err)
		}
	}
}
