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
	if lock, err := record.LockContext(ctx, target, 30*time.Second); !errors.Is(err, context.DeadlineExceeded) || lock != nil {
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
	if exclusive, err := record.PromoteContext(ctx, target, 30*time.Second); !errors.Is(err, context.DeadlineExceeded) || exclusive != nil {
		t.Fatalf("the promotion lock wait: %v", err)
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Fatalf("the promotion lock wait outlived its context by %v", took)
	}
	done, cancelled := context.WithCancel(context.Background())
	cancelled()
	if _, err := record.PromoteContext(done, filepath.Join(dir, "other.json"), 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("a context already done takes nothing: %v", err)
	}
}
