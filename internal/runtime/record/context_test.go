package record_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
)

// A wait for a lock another run holds ends when the caller's context does, with the context's
// error and the lock not taken: an interrupted command never goes on to take it once it frees.
func TestLockWaitsEndWithTheContext(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "host-record.json")
	if err := os.WriteFile(target+record.LockSuffix, []byte("4242"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start := time.Now()
	if lock, err := record.Lock(ctx, target, 30*time.Second); !errors.Is(err, context.DeadlineExceeded) || lock != nil {
		t.Fatalf("the .crw-lock wait: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the .crw-lock wait outlived its context by %v", took)
	}

	held, err := os.OpenFile(target+record.PromotionLockSuffix, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start = time.Now()
	if exclusive, err := record.Promote(ctx, target, 30*time.Second); !errors.Is(err, context.DeadlineExceeded) || exclusive != nil {
		t.Fatalf("the promotion lock wait: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the promotion lock wait outlived its context by %v", took)
	}
	done, cancelled := context.WithCancel(context.Background())
	cancelled()
	if _, err := record.Promote(done, filepath.Join(dir, "other.json"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("a context already done takes nothing: %v", err)
	}
}

// A context carrying a record.Seam steers a lock wait without the real clock: the seam is told
// each time a wait is about to pause, and its clock decides when the wait is over. Both waits take
// the same steps, the .crw-lock one and the promotion one, and a context without a seam is
// unchanged (TestLockWaitsEndWithTheContext).
func TestASeamSteersALockWait(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "host-record.json")
	if err := os.WriteFile(target+record.LockSuffix, []byte("4242"), 0o644); err != nil {
		t.Fatal(err)
	}
	held, err := os.OpenFile(target+record.PromotionLockSuffix, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	if err := unix.Flock(int(held.Fd()), unix.LOCK_EX); err != nil {
		t.Fatal(err)
	}

	// The clock moves a day when the wait first pauses: the wait is over at once, not after its hour.
	started := time.Now()
	var passed time.Duration
	var paused []string
	ctx := record.WithSeam(context.Background(), record.Seam{
		Now:     func() time.Time { return started.Add(passed) },
		Waiting: func(path string) { paused = append(paused, path); passed = 24 * time.Hour },
	})
	var busy *record.Busy
	if lock, err := record.Lock(ctx, target, time.Hour); !errors.As(err, &busy) || lock != nil {
		t.Fatalf("the .crw-lock wait: %v", err)
	}
	passed = 0
	if exclusive, err := record.Promote(ctx, target, time.Hour); !errors.As(err, &busy) || exclusive != nil {
		t.Fatalf("the promotion lock wait: %v", err)
	}
	if want := []string{target + record.LockSuffix, target + record.PromotionLockSuffix}; len(paused) != 2 || paused[0] != want[0] || paused[1] != want[1] {
		t.Fatalf("the seam was told of the pauses %v, want %v", paused, want)
	}

	// Cancelling from the seam ends the wait with the context's error, with nothing taken.
	cancelling, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancelling = record.WithSeam(cancelling, record.Seam{Waiting: func(string) { cancel() }})
	if exclusive, err := record.Promote(cancelling, target, time.Hour); !errors.Is(err, context.Canceled) || exclusive != nil {
		t.Fatalf("a wait cancelled as it paused: %v", err)
	}
}
