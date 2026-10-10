//go:build linux

package store

import (
	"runtime"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// processorClock says whether threadCPU reads the thread's processor time (it does on Linux), so the frozen-document test
// asserts its ratio.
const processorClock = true

// threadCPU is the processor time the calling thread has used. It does not count the time the thread is descheduled, so a
// host under load stretches it far less than a wall clock; the caller keeps the goroutine on its thread.
func threadCPU(tb testing.TB) time.Duration {
	tb.Helper()
	var now unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_THREAD_CPUTIME_ID, &now); err != nil {
		tb.Fatalf("clock_gettime: %v", err)
	}
	return time.Duration(now.Nano())
}

// pinThread keeps the calling goroutine on one thread until the test ends, so threadCPU measures it.
func pinThread(tb testing.TB) {
	tb.Helper()
	runtime.LockOSThread()
	tb.Cleanup(runtime.UnlockOSThread)
}
