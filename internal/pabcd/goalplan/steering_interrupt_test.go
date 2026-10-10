package goalplan

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// CRW-1074: the goalplan write lock takes the invocation's context for the loop steer row. The first SIGINT
// ends the lock wait, the context is read once more with the lock held and the plan read (immediately before
// the steering transaction's first write), and a steer they end writes nothing and holds nothing. Once the
// plan write has begun the transaction finishes and answers as before.

// steeringInterruptBytes is what the plan directory holds: the plan and ledger files, by name.
func steeringInterruptBytes(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			out[entry.Name()] = "dir"
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		out[entry.Name()] = string(raw)
	}
	return out
}

func steeringInterruptSame(t *testing.T, before, after map[string]string) {
	t.Helper()
	if len(before) != len(after) {
		t.Fatalf("the plan directory changed: %v -> %v", steeringInterruptNames(before), steeringInterruptNames(after))
	}
	for name, body := range before {
		if after[name] != body {
			t.Fatalf("%s changed", name)
		}
	}
}

func steeringInterruptNames(m map[string]string) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestSteeringInterruptEndedContextWritesNothing(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	dir := steeringApplyDir(t, cwd, slug)
	before := steeringInterruptBytes(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), &SteeringBatchOptions{Lock: &GoalplanWriteLockOptions{Context: ctx}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("ended context: %v, want context.Canceled", err)
	}
	steeringInterruptSame(t, before, steeringInterruptBytes(t, dir))
}

func TestSteeringInterruptEndsTheLockWait(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	dir := steeringApplyDir(t, cwd, slug)
	if err := os.Mkdir(filepath.Join(dir, GoalplanLockDir), 0o755); err != nil { // another holder
		t.Fatal(err)
	}
	before := steeringInterruptBytes(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A wait that the context does not end would sleep these delays out: 30 s.
	lock := &GoalplanWriteLockOptions{Context: ctx, RetryDelaysMs: []int{10_000, 10_000, 10_000}}
	go func() { time.Sleep(30 * time.Millisecond); cancel() }()
	started := time.Now()
	_, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), &SteeringBatchOptions{Lock: lock})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled wait: %v, want context.Canceled", err)
	}
	if took := time.Since(started); took > 5*time.Second {
		t.Fatalf("the wait took %v after the cancel", took)
	}
	steeringInterruptSame(t, before, steeringInterruptBytes(t, dir)) // the holder's lock directory is still there
}

// TestSteeringInterruptCancelledWithTheLockHeldWritesNothing cancels the context from the lock's Now seam,
// which runs right after the lock is taken: the pre-write check then ends the steer, and the lock is released.
func TestSteeringInterruptCancelledWithTheLockHeldWritesNothing(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	dir := steeringApplyDir(t, cwd, slug)
	before := steeringInterruptBytes(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	lock := &GoalplanWriteLockOptions{Context: ctx, Now: func() string { cancel(); return "2026-03-03T00:00:00.000Z" }}
	_, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), &SteeringBatchOptions{Lock: lock})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled with the lock held: %v, want context.Canceled", err)
	}
	steeringInterruptSame(t, before, steeringInterruptBytes(t, dir))
}

// TestSteeringInterruptCancelledAfterThePlanWriteAnswersAsToday cancels right after the rename that publishes
// the plan: the first write has been done, so the transaction finishes, writes its ledger row and answers applied.
func TestSteeringInterruptCancelledAfterThePlanWriteAnswersAsToday(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	dir := steeringApplyDir(t, cwd, slug)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), &SteeringBatchOptions{
		Lock:    &GoalplanWriteLockOptions{Context: ctx},
		publish: &goalplanPublishedOptions{AfterRename: cancel},
	})
	if err != nil {
		t.Fatalf("a steer past its first write returned %v", err)
	}
	if result.Kind != SteerResultApplied {
		t.Fatalf("kind = %q (%q), want applied", result.Kind, result.Reason)
	}
	if len(ReadGoalplan(cwd, slug).SteeringLog) != 1 {
		t.Fatal("the published plan does not hold the steering entry")
	}
	if raw, err := os.ReadFile(filepath.Join(dir, GoalplanLedgerFile)); err != nil || len(raw) == 0 {
		t.Fatalf("the ledger row of a steer past its first write is missing: %v", err)
	}
}

// TestSteeringInterruptLiveContextIsTheControl: a context that never ends changes nothing.
func TestSteeringInterruptLiveContextIsTheControl(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), &SteeringBatchOptions{Lock: &GoalplanWriteLockOptions{Context: ctx}})
	if err != nil || result.Kind != SteerResultApplied {
		t.Fatalf("live context: %v %+v", err, result)
	}
}

// TestSteeringInterruptCancelledWhilePreparingTheChangeWritesNothing cancels from the batch's clock, which runs
// after the lock is taken and before the plan write: the change is prepared and the context read again just
// before the first write, so nothing is published, no ledger row is written and the lock is released.
func TestSteeringInterruptCancelledWhilePreparingTheChangeWritesNothing(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	dir := steeringApplyDir(t, cwd, slug)
	before := steeringInterruptBytes(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), &SteeringBatchOptions{
		Lock: &GoalplanWriteLockOptions{Context: ctx},
		Now:  func() string { cancel(); return "2026-03-03T00:00:00.000Z" },
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled while preparing: %v, want context.Canceled", err)
	}
	steeringInterruptSame(t, before, steeringInterruptBytes(t, dir))
}

// TestSteeringBeforeWriteRunsOnceAsThePlanWriteBegins: the hook runs once for a batch that writes, with the plan
// still unwritten, and never for a duplicate that writes nothing.
func TestSteeringBeforeWriteRunsOnceAsThePlanWriteBegins(t *testing.T) {
	cwd, slug := steeringApplyWorkspace(t)
	calls, logged := 0, -1
	options := &SteeringBatchOptions{BeforeWrite: func() {
		calls++
		logged = len(ReadGoalplan(cwd, slug).SteeringLog)
	}}
	result, err := ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), options)
	if err != nil || result.Kind != SteerResultApplied {
		t.Fatalf("first batch: %+v %v", result, err)
	}
	if calls != 1 || logged != 0 {
		t.Fatalf("BeforeWrite ran %d times, saw %d steering entries; want once, before the write", calls, logged)
	}
	result, err = ApplySteeringBatch(cwd, slug, steeringApplyBatch(nil), options)
	if err != nil || result.Kind != SteerResultDuplicate {
		t.Fatalf("second batch: %+v %v", result, err)
	}
	if calls != 1 {
		t.Fatalf("BeforeWrite ran for a duplicate: %d calls", calls)
	}
}

// steeringRetryWithLostRows applies steeringPayloadDepsBatch with the plan's ledger unwritable and repairs the ledger, so a retry of
// the same key owes the rows the first attempt lost. It answers the plan directory's state before the retry.
func steeringRetryWithLostRows(t *testing.T) (cwd, slug string, before map[string]string) {
	t.Helper()
	cwd, slug = steeringApplyWorkspace(t)
	dir := steeringApplyDir(t, cwd, slug)
	ledger := filepath.Join(dir, GoalplanLedgerFile)
	if err := os.Mkdir(ledger, 0o777); err != nil {
		t.Fatal(err)
	}
	steeringApply(t, cwd, slug, steeringPayloadDepsBatch(), nil, SteerResultApplied)
	if err := os.Remove(ledger); err != nil {
		t.Fatal(err)
	}
	return cwd, slug, steeringInterruptBytes(t, dir)
}

// Red on 3fceb240: a retry that owed rows checked the context before it scanned the ledger and never after, so an invocation ended
// during the scan still wrote every missing row and then answered as an interrupted one.
func TestSteeringRetryCancelledDuringTheScanWritesNothing(t *testing.T) {
	cwd, slug, before := steeringRetryWithLostRows(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := ApplySteeringBatch(cwd, slug, steeringPayloadDepsBatch(), &SteeringBatchOptions{
		Lock:      &GoalplanWriteLockOptions{Context: ctx},
		afterScan: cancel,
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled during the scan: %v, want context.Canceled", err)
	}
	steeringInterruptSame(t, before, steeringInterruptBytes(t, steeringApplyDir(t, cwd, slug)))
}

// Red on 3fceb240: the write-start callback never ran for a retry that records rows, so the caller took a retry that had written
// for one that had not begun.
func TestSteeringBeforeWriteRunsOnceForARetryThatRecordsRows(t *testing.T) {
	cwd, slug, _ := steeringRetryWithLostRows(t)
	calls, rowsAtCall := 0, -1
	result, err := ApplySteeringBatch(cwd, slug, steeringPayloadDepsBatch(), &SteeringBatchOptions{BeforeWrite: func() {
		calls++
		rowsAtCall = len(steeringApplyRows(t, cwd, slug))
	}})
	if err != nil || result.Kind != SteerResultDuplicate || result.Warning != "" {
		t.Fatalf("retry: %+v %v", result, err)
	}
	if calls != 1 || rowsAtCall != 0 {
		t.Fatalf("BeforeWrite ran %d times with %d rows already written; want once, before the first row", calls, rowsAtCall)
	}
	if steered, deps := steeringPayloadCounts(t, cwd, slug); steered != 1 || deps != 2 {
		t.Fatalf("rows after the retry: %d steered, %d dependency_registered", steered, deps)
	}
	// Everything is recorded now: a third call writes nothing and does not announce a write.
	again := 0
	if result, err = ApplySteeringBatch(cwd, slug, steeringPayloadDepsBatch(), &SteeringBatchOptions{BeforeWrite: func() { again++ }}); err != nil || result.Kind != SteerResultDuplicate || again != 0 {
		t.Fatalf("third call: %+v %v (BeforeWrite %d times)", result, err, again)
	}
}
