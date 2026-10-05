package metric

import (
	"context"
	"os"
	"sync/atomic"
	"testing"
)

// CRW-637: a cancellation that lands while the ingest waits for the ledger lock must end the record before the row is
// written. CRW-627 ends the wait with the context, but the wait only looks at the context where it sleeps: the select
// takes its timer branch whenever the timer is ready, and Go picks any ready case, so a cancellation that arrives with
// the timer still lets the next Flock attempt run, and the holder releasing the lock in that moment hands the lock to a
// waiter that has already been cancelled. The oracle has no lock and no signal handler, so its process dies at the
// interrupt and never writes that row; these cases hold the same line.
//
// The two seams are the check that follows the select's timer branch and the check that follows a lock taken after a
// refused attempt. Both are observed through metricsTimerOnly, which makes the race deterministic: a real context
// cannot be made to lose the both-ready race on demand, and a probabilistic race cannot be relied on to leave the
// unfixed code red with a row actually written.

// metricsTimerOnly is a context whose Done channel never closes and whose Err answers nil for the first errAfter asks
// and context.Canceled from then on, so the lock wait's select can only take its timer branch and the cancellation lands
// exactly where the ask count puts it. Done asks are counted as in metricsWaiting: the first is the check for a context
// that can never end, the second is the select that follows a refused attempt, so waiting closes with the wait parked.
type metricsTimerOnly struct {
	context.Context
	asks     atomic.Int32
	errAsks  atomic.Int32
	errAfter int32
	waiting  chan struct{}
	done     chan struct{}
}

func (c *metricsTimerOnly) Done() <-chan struct{} {
	if c.asks.Add(1) == 2 {
		close(c.waiting)
	}
	return c.done // never closed: the select has only the timer branch to take
}

func (c *metricsTimerOnly) Err() error {
	if c.errAsks.Add(1) > c.errAfter {
		return context.Canceled
	}
	return nil
}

// The ask counts of the two seams, as the fixed code makes them: the per-line check is ask 1 and the check after the
// select is ask 2, so a cancellation from ask 2 ends the wait and a cancellation from ask 3 lands after the lock.
const (
	metricsAfterTheTimerBranch = 1
	metricsAfterTheLock        = 2
)

func TestRecordMetricsFromTextContextCancelledAfterTheTimerBranchWritesNoRow(t *testing.T) {
	metricsLockCancel(t, metricsAfterTheTimerBranch)
}

func TestRecordMetricsFromTextContextCancelledAfterTheLockIsTakenWritesNoRow(t *testing.T) {
	metricsLockCancel(t, metricsAfterTheLock)
}

// metricsLockCancel drives one ingest of two METRIC lines against a ledger another descriptor holds, cancels it at the
// ask the seam names, and releases the holder while the wait sleeps on its first tick. The cancelled record must return
// the context's own error with no row written and the file closed.
func metricsLockCancel(t *testing.T, errAfter int32) {
	t.Helper()
	cwd := t.TempDir()
	release := metricsHoldLedger(t, cwd)
	before, err := os.ReadFile(metricsPath(cwd))
	metricsMust(t, err)
	held := metricsLedgerDescriptors(t, cwd) // the holder's descriptor; the ingest's is opened later and closed again
	ctx := &metricsTimerOnly{Context: context.Background(), errAfter: errAfter, waiting: make(chan struct{}), done: make(chan struct{})}
	done, _ := metricsStart(func(in TextInput) ([]Record, error) { return RecordMetricsFromTextContext(ctx, cwd, in) })
	metricsReach(t, ctx.waiting, done, release, "the lock wait")
	release()
	got := metricsWithin(t, done, release)
	after, _ := os.ReadFile(metricsPath(cwd))
	// The give-up returns the context's own error as it is, not a join that merely wraps it.
	if got.err != context.Canceled || len(got.rows) != 0 || string(after) != string(before) {
		t.Fatalf("cancelled from ask %d: error %v, rows %v, ledger now %q, was %q; want no rows, Canceled and no write", errAfter, got.err, got.rows, after, before)
	}
	if now := metricsLedgerDescriptors(t, cwd); held > 0 && now != held-1 { // the holder released its own as well
		t.Errorf("the process holds %d descriptors on the ledger after the give-up and held %d while the holder had it: the file was not closed", now, held)
	}
}
