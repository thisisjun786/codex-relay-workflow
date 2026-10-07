package configguard

import (
	"sync"
	"syscall"
	"testing"
	"time"
)

// execwriteLockObserveWindow bounds the negative observation only: the helper's own write takes
// microseconds, so a write that does not take the fork lock closes done far inside this window.
const execwriteLockObserveWindow = 500 * time.Millisecond

// execwriteLockFinishWindow bounds the positive observation: the write must finish once the fork
// lock is released. It is a hang ceiling, not the pass condition.
const execwriteLockFinishWindow = 30 * time.Second

// TestSelfHealReportWriteFakeCodexWaitsForTheForkLock is CRW-942's regression. The helpers write a
// fake codex this test process then runs (selfheal_report.go:263, exec.CommandContext), and a fork
// that lands inside such a write inherits the open-for-writing descriptor, which leaves the path
// unexecutable until the child execs (ETXTBSY, golang/go#22315). The write must therefore hold
// syscall.ForkLock for reading, and this test proves it: while another goroutine holds the lock for
// writing, the helper cannot finish.
//
// This test and the package it lives in must never call t.Parallel. Holding ForkLock for writing
// starves every later fork and exec in this process, and this package's own tests fork
// (selfheal_report.go:263). The lock is held only for the bounded window below and is released on
// every exit path.
func TestSelfHealReportWriteFakeCodexWaitsForTheForkLock(t *testing.T) {
	dir := t.TempDir()

	lockHeld := make(chan struct{})
	lockReleased := make(chan struct{})
	go func() {
		syscall.ForkLock.Lock()
		close(lockHeld)
		<-lockReleased
		syscall.ForkLock.Unlock()
	}()
	<-lockHeld
	var once sync.Once
	release := func() { once.Do(func() { close(lockReleased) }) }
	defer release()

	started := make(chan struct{})
	done := make(chan struct{})
	go func() {
		close(started)
		selfHealReportWriteFakeCodex(t, dir, "exit 0\n")
		close(done)
	}()
	<-started

	select {
	case <-done:
		release()
		t.Fatal("selfHealReportWriteFakeCodex finished while syscall.ForkLock was held for writing: the write does not take the fork lock")
	case <-time.After(execwriteLockObserveWindow):
	}

	release()
	select {
	case <-done:
	case <-time.After(execwriteLockFinishWindow):
		t.Fatal("selfHealReportWriteFakeCodex did not finish after syscall.ForkLock was released")
	}
}
