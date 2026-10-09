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
