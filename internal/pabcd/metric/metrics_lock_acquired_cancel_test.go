package metric

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

// CRW-667: the context must be read once more after the ledger lock is taken, whether or not the
// wait had to wait for it. CRW-637 checks the lock only when the wait took it after a refused
// attempt, so a lock the first non-blocking Flock takes at once (a free ledger) is written with no
// look at the context: an ingest cancelled right after that Flock leaves a row and answers nil.
// The oracle has no lock and no signal handler, so its process dies at the interrupt and never
// writes that row (pabcd-state/src/metrics.ts:119-137 at v0.2.40); the ledger lock is this port's
// own, so the fix is port-introduced (docs/port-cxc/known-defects.md).

// metricsAcquiredCancelAfterTheFirstFlock is a context whose Done channel never closes and whose
// Err answers nil for the first errAfter asks and context.Canceled from then on. Done is a live
// channel, so metricLockWait takes its non-blocking path; on a free ledger the first Flock(LOCK_NB)
// succeeds without asking Err, so ask 1 is the per-line check in RecordMetricsFromTextContext and
// ask 2 the post-lock check in appendRow. Pinning errAfter to the ask before the post-lock check
// fixes the cancellation exactly where the case needs it, the way metricsTimerOnly pins CRW-637's
// seams.
type metricsAcquiredCancelAfterTheFirstFlock struct {
	context.Context
	done     chan struct{}
	errAsks  atomic.Int32
	errAfter int32
}

func (c *metricsAcquiredCancelAfterTheFirstFlock) Done() <-chan struct{} { return c.done }

func (c *metricsAcquiredCancelAfterTheFirstFlock) Err() error {
	if c.errAsks.Add(1) > c.errAfter {
		return context.Canceled
	}
	return nil
}

// metricsAcquiredPostLockAsk is the ask whose answer is the post-lock check's: on a free ledger the
// first Flock succeeds without an Err ask, so ask 1 is the per-line check and ask 2 this one.
const metricsAcquiredPostLockAsk = 2

// metricsAcquiredTempHomes points HOME, CODEX_HOME and CRW_HOME at a temporary directory, as every
// case that can reach config or record code must, so nothing reads or writes a real home.
func metricsAcquiredTempHomes(t *testing.T) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("CODEX_HOME", filepath.Join(home, "codex"))
	t.Setenv("CRW_HOME", filepath.Join(home, "crw"))
}

func TestRecordMetricsFromTextContextCancelledAfterTheFirstFlockWritesNoRow(t *testing.T) {
	metricsAcquiredCancelAfterTheFirstFlockWritesNoRow(t, "METRIC a=1\n")
}

func TestRecordMetricsFromTextContextCancelledAfterTheFirstFlockWithTwoLinesWritesNoRow(t *testing.T) {
	metricsAcquiredCancelAfterTheFirstFlockWritesNoRow(t, metricsTwoLines)
}

// metricsAcquiredCancelAfterTheFirstFlockWritesNoRow records the text on a fresh, free ledger with a
// context cancelled at the post-lock check (ask 2) and requires no row and the context's own error.
// On the unfixed code the free lock writes the first line, so both cases fail there: one line
// answers a row and nil, two lines leave a in the ledger.
func metricsAcquiredCancelAfterTheFirstFlockWritesNoRow(t *testing.T, text string) {
	t.Helper()
	metricsAcquiredTempHomes(t)
	cwd := t.TempDir()
	ctx := &metricsAcquiredCancelAfterTheFirstFlock{
		Context:  context.Background(),
		done:     make(chan struct{}),
		errAfter: metricsAcquiredPostLockAsk - 1,
	}
	rows, err := RecordMetricsFromTextContext(ctx, cwd, TextInput{SessionID: "s", Text: text, Source: EvaluateSh})
	if err != context.Canceled || len(rows) != 0 {
		t.Fatalf("cancelled after the first Flock: error %v, rows %v; want no rows and Canceled", err, rows)
	}
	if ledger := ReadObjectiveMetrics(cwd, "s"); len(ledger) != 0 {
		t.Fatalf("cancelled after the first Flock left %d ledger row(s): %v", len(ledger), ledger)
	}
	if raw, readErr := os.ReadFile(metricsPath(cwd)); readErr == nil && len(raw) != 0 {
		t.Fatalf("cancelled after the first Flock wrote to the ledger: %q", raw)
	} else if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		t.Fatalf("reading the ledger after the cancelled append: %v", readErr)
	}
}
