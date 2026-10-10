//go:build !linux

package store

import (
	"testing"
	"time"
)

// processorClock says whether threadCPU reads the thread's processor time. It does not off Linux: no thread CPU clock is read
// here, so the frozen-document test does not assert its ratio at all (a ratio of two wall-clock spans is the very measure a
// loaded host inflates), and only logs the measurement and keeps the correctness checks and the hang guard.
const processorClock = false

var threadCPUStart = time.Now()

// threadCPU is a wall clock here, only for the logged measurement; nothing asserts on it.
func threadCPU(testing.TB) time.Duration { return time.Since(threadCPUStart) }

func pinThread(testing.TB) {}
