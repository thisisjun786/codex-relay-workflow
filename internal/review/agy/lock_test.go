package agy

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestOnlyOneCallAtATime(t *testing.T) {
	a, recA := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"a"}`, Sleep: 300 * time.Millisecond})
	b, recB := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"b"}`, Sleep: 300 * time.Millisecond})
	b.LockPath = a.LockPath
	var wg sync.WaitGroup
	results, errs := make([]Result, 2), make([]error, 2)
	for i, cfg := range []Config{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i], errs[i] = try(cfg, Request{Prompt: []byte("hi")})
		}()
	}
	wg.Wait()
	first, second := recA(), recB()
	if second.Start < first.Start {
		first, second = second, first
	}
	if second.Start < first.End {
		t.Errorf("the calls overlapped: first %d-%d, second started %d", first.Start, first.End, second.Start)
	}
	for i, res := range results {
		if errs[i] != nil || res.Class != ClassNormal {
			t.Errorf("call %d: %v, %s/%s: %s", i, errs[i], res.Class, res.Reason, res.Detail)
		}
	}
}

// TestHeldLock: with the lock file locked by someone else (a descriptor of the test, as another process would hold it), a call waits for at most its wait limit,
// gets a defined outcome without starting agy, can be abandoned by its caller, and runs as soon as the lock is free.
func TestHeldLock(t *testing.T) {
	cfg, _ := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"PONG"}`})
	fd, err := unix.Open(cfg.LockPath, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC, 0o600)
	if err != nil || unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		t.Fatalf("could not hold the lock: %v", err)
	}
	for _, wait := range []time.Duration{150 * time.Millisecond, -1} {
		cfg.LockWait = wait
		res := run(t, cfg, Request{Prompt: []byte("hi")})
		if res.Class != ClassUnavailable || res.Reason != ReasonLockWaitExpired || res.ExitCode != -1 || (wait > 0 && res.Waited < wait) {
			t.Errorf("wait %s: %s/%s exit %d waited %s: %s", wait, res.Class, res.Reason, res.ExitCode, res.Waited, res.Detail)
		}
	}
	if _, err := os.Stat(recordPath(cfg)); err == nil {
		t.Error("agy ran although the lock was held")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	cfg.LockWait = time.Minute
	if _, err := Run(ctx, cfg, Request{Prompt: []byte("hi")}); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("a caller that gives up while waiting: %v", err)
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	_ = unix.Close(fd)
	if res := run(t, cfg, Request{Prompt: []byte("hi")}); res.Class != ClassNormal {
		t.Errorf("after the lock was freed: %s/%s: %s", res.Class, res.Reason, res.Detail)
	}
}
