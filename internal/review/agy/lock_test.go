package agy

import (
	"os"
	"sync"
	"testing"
	"time"
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
	if results[0].Waited < 200*time.Millisecond && results[1].Waited < 200*time.Millisecond {
		t.Errorf("one call must have waited for the other: %s, %s", results[0].Waited, results[1].Waited)
	}
}

// TestLockWaitExpires: while one call holds the lock, a second one that is not willing to wait long enough gets a defined outcome and never starts agy.
func TestLockWaitExpires(t *testing.T) {
	for _, wait := range []time.Duration{150 * time.Millisecond, -1} {
		holder, _ := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"a"}`, Sleep: 1500 * time.Millisecond})
		waiter, _ := fakeCfg(t, fakeSpec{Stdout: `{"status":"SUCCESS","response":"b"}`})
		waiterRecord := recordPath(waiter)
		waiter.LockPath, waiter.LockWait = holder.LockPath, wait
		type outcome struct {
			res Result
			err error
		}
		done := make(chan outcome)
		go func() {
			res, err := try(holder, Request{Prompt: []byte("hi")})
			done <- outcome{res, err}
		}()
		for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
			if _, err := os.Stat(recordPath(holder)); err == nil {
				break // the holder's agy is running, so it holds the lock
			}
		}
		res := run(t, waiter, Request{Prompt: []byte("hi")})
		if res.Class != ClassUnavailable || res.Reason != ReasonLockWaitExpired || res.ExitCode != -1 {
			t.Errorf("wait %s: %s/%s exit %d: %s", wait, res.Class, res.Reason, res.ExitCode, res.Detail)
		}
		if wait > 0 && res.Waited < wait {
			t.Errorf("waited %s, want at least %s", res.Waited, wait)
		}
		if _, err := os.Stat(waiterRecord); err == nil {
			t.Errorf("wait %s: the waiter's agy ran although the lock was held", wait)
		}
		if h := <-done; h.err != nil || h.res.Class != ClassNormal {
			t.Errorf("the holder: %v, %s/%s", h.err, h.res.Class, h.res.Reason)
		}
	}
}
