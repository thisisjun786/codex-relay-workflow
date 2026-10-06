package metric

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// CRW-627: the record window of an ingest ends with the invocation's context. The oracle has no lock and no signal handler: its
// process dies at the first SIGINT and keeps the lines it had appended. These cases hold the ledger lock the way another writer
// does, through a descriptor of their own, and cancel at the moments the wait and the loop can honour.

const metricsTwoLines = "METRIC a=1\nMETRIC b=2\n"

// metricsOutcome is what a record call answered.
type metricsOutcome struct {
	rows []Record
	err  error
}

// metricsHoldLedger writes a ledger with one row of another session and locks it exclusively through a descriptor opened without
// O_APPEND. It returns the release; the test's cleanup releases too.
func metricsHoldLedger(t *testing.T, cwd string) (release func()) {
	t.Helper()
	prior := Record{TS: "2026-01-01T00:00:00.000Z", SessionID: "p", WorkPhaseID: DefaultWorkPhaseID, MetricName: "prior", Value: 1, Baseline: 1, Best: 1, Source: EvaluateSh}
	metricsWrite(t, cwd, MetricsFile, []byte(Encode(prior)+"\n"))
	f, err := os.OpenFile(metricsPath(cwd), os.O_WRONLY, 0)
	metricsMust(t, err)
	metricsMust(t, unix.Flock(int(f.Fd()), unix.LOCK_EX))
	var once sync.Once
	release = func() { once.Do(func() { f.Close() }) } // closing drops the lock
	t.Cleanup(release)
	return release
}

// metricsWaiting is a context that closes waiting when the lock wait asks for Done() the second time: the first ask is the check for a
// context that can never end, the second is the select that follows a refused non-blocking attempt, so the lock has been seen held.
// It ties these cases to that call order; a change to it fails them at their bounded wait instead of passing them wrongly.
type metricsWaiting struct {
	context.Context
	asks    atomic.Int32
	waiting chan struct{}
}

func (c *metricsWaiting) Done() <-chan struct{} {
	if c.asks.Add(1) == 2 {
		close(c.waiting)
	}
	return c.Context.Done()
}

// metricsStart records the two METRIC lines in a goroutine. entered closes when the first record is under way: TextInput.Now runs
// inside it, after the check before the line and before the append.
func metricsStart(record func(TextInput) ([]Record, error)) (done <-chan metricsOutcome, entered <-chan struct{}) {
	out, in := make(chan metricsOutcome, 1), make(chan struct{})
	var once sync.Once
	input := TextInput{SessionID: "s", Text: metricsTwoLines, Source: EvaluateSh, Now: func() string {
		once.Do(func() { close(in) })
		return "2026-01-02T00:00:00.000Z"
	}}
	go func() {
		rows, err := record(input)
		out <- metricsOutcome{rows, err}
	}()
	return out, in
}

// metricsReach waits until signal closes, which is the call under test getting where the case needs it, and fails after 5 s. The
// held lock is released and the call let end first, so no writer outlives the case.
func metricsReach(t *testing.T, signal <-chan struct{}, done <-chan metricsOutcome, release func(), where string) {
	t.Helper()
	select {
	case <-signal:
	case got := <-done:
		t.Fatalf("the ingest ended before %s: rows %v, error %v", where, got.rows, got.err)
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatalf("the ingest did not reach %s within 5 s", where)
	}
}

// metricsWithin is the outcome of a call that must end by itself. When it does not, the held lock is released so the call can end,
// and the test fails once it has.
func metricsWithin(t *testing.T, done <-chan metricsOutcome, release func()) metricsOutcome {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(5 * time.Second):
		release()
		<-done
		t.Fatal("the ingest did not end within 5 s")
		return metricsOutcome{}
	}
}

// metricsLedgerDescriptors is how many descriptors this process holds on the ledger, or -1 where /proc does not show them.
func metricsLedgerDescriptors(t *testing.T, cwd string) int {
	t.Helper()
	ledger, err := filepath.EvalSymlinks(metricsPath(cwd))
	metricsMust(t, err)
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return -1
	}
	n := 0
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && target == ledger {
			n++
		}
	}
	return n
}

// metricsWantBoth checks that a call that waited out another writer's lock recorded both lines after it.
func metricsWantBoth(t *testing.T, cwd string, got metricsOutcome) {
	t.Helper()
	if rows := ReadObjectiveMetrics(cwd, "s"); got.err != nil || len(got.rows) != 2 || len(rows) != 2 {
		t.Fatalf("ingest that waited for the lock: error %v, rows %v, ledger rows %v; want both rows and no error", got.err, got.rows, rows)
	}
}

func TestRecordMetricsFromTextContextStopsBetweenLines(t *testing.T) {
	cwd := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The interrupt lands inside the first record: Now runs there, after the check before the line and before the append. The lock is
	// free, but the context is read once more after it is taken (CRW-667), so that row is not written either and the run ends with
	// Canceled.
	rows, err := RecordMetricsFromTextContext(ctx, cwd, TextInput{SessionID: "s", Text: metricsTwoLines, Source: EvaluateSh, Now: func() string {
		cancel()
		return "2026-01-02T00:00:00.000Z"
	}})
	if got := ReadObjectiveMetrics(cwd, "s"); !errors.Is(err, context.Canceled) || len(rows) != 0 || len(got) != 0 {
		t.Fatalf("cancelled between lines: error %v, rows %v, ledger %v; want no rows and Canceled", err, rows, got)
	}
}

func TestRecordMetricsFromTextContextGivesUpWhileTheLedgerIsLocked(t *testing.T) {
	cwd := t.TempDir()
	release := metricsHoldLedger(t, cwd)
	before, err := os.ReadFile(metricsPath(cwd))
	metricsMust(t, err)
	held := metricsLedgerDescriptors(t, cwd)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &metricsWaiting{Context: base, waiting: make(chan struct{})}
	done, _ := metricsStart(func(in TextInput) ([]Record, error) { return RecordMetricsFromTextContext(ctx, cwd, in) })
	metricsReach(t, ctx.waiting, done, release, "the lock wait")
	cancel()
	got := metricsWithin(t, done, release)
	after, _ := os.ReadFile(metricsPath(cwd))
	// The give-up returns the context's own error as it is, not a join that merely wraps it.
	if got.err != context.Canceled || len(got.rows) != 0 || string(after) != string(before) {
		t.Fatalf("ingest cancelled in the lock wait: error %v, rows %v, ledger now %q, was %q", got.err, got.rows, after, before)
	}
	if now := metricsLedgerDescriptors(t, cwd); now != held {
		t.Errorf("the process holds %d descriptors on the ledger after the give-up and held %d before it: the file was not closed", now, held)
	}
	release()
	again, _ := metricsStart(func(in TextInput) ([]Record, error) { return RecordMetricsFromText(cwd, in) })
	if got := metricsWithin(t, again, release); got.err != nil || len(got.rows) != 2 {
		t.Fatalf("ingest after the give-up: error %v, rows %v; want both rows", got.err, got.rows)
	}
}

func TestRecordMetricsFromTextContextRetriesWhileTheLedgerIsLocked(t *testing.T) {
	cwd := t.TempDir()
	release := metricsHoldLedger(t, cwd)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &metricsWaiting{Context: base, waiting: make(chan struct{})}
	done, _ := metricsStart(func(in TextInput) ([]Record, error) { return RecordMetricsFromTextContext(ctx, cwd, in) })
	metricsReach(t, ctx.waiting, done, release, "the lock wait")
	release()
	metricsWantBoth(t, cwd, metricsWithin(t, done, release))
}

// A caller whose context can never end keeps the kernel wait it always had: Done() is nil, so the lock is asked for once, blocking.
// This is a smoke case: a build that polled for Background too would pass it, so that branch is checked in review.
func TestRecordMetricsWaitForALedgerLockWithoutAContextThatCanEnd(t *testing.T) {
	for name, record := range map[string]func(cwd string, in TextInput) ([]Record, error){
		"RecordMetricsFromText": RecordMetricsFromText,
		"Context with Background": func(cwd string, in TextInput) ([]Record, error) {
			return RecordMetricsFromTextContext(context.Background(), cwd, in)
		},
	} {
		t.Run(name, func(t *testing.T) {
			cwd := t.TempDir()
			release := metricsHoldLedger(t, cwd)
			done, entered := metricsStart(func(in TextInput) ([]Record, error) { return record(cwd, in) })
			metricsReach(t, entered, done, release, "its first record")
			time.Sleep(50 * time.Millisecond) // lets it reach the lock
			release()
			metricsWantBoth(t, cwd, metricsWithin(t, done, release))
		})
	}
}

func TestRecordMetricsFromTextContextUnderAnEndedContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cwd := t.TempDir()
	if rows, err := RecordMetricsFromTextContext(ctx, cwd, TextInput{SessionID: "s", Text: "no metric line here\n", Source: EvaluateSh}); err != nil || rows == nil || len(rows) != 0 {
		t.Errorf("no METRIC line: rows %v, error %v; want an empty slice and no error", rows, err)
	}
	rows, err := RecordMetricsFromTextContext(ctx, cwd, TextInput{SessionID: "s", Text: metricsTwoLines, Source: EvaluateSh})
	if _, statErr := os.Stat(metricsPath(cwd)); !errors.Is(err, context.Canceled) || len(rows) != 0 || !errors.Is(statErr, fs.ErrNotExist) {
		t.Errorf("two METRIC lines under an ended context: rows %v, error %v, ledger stat %v; want no rows, Canceled and no ledger", rows, err, statErr)
	}
}
