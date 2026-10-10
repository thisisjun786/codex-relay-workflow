//go:build !linux

package store

import (
	"testing"
	"time"
)

var threadCPUStart = time.Now()

// threadCPU is a wall clock where the thread's processor time is not read (it is on Linux): the ratio the tests take of it
// is looser there than a loaded host needs, which only this fallback pays for.
func threadCPU(testing.TB) time.Duration { return time.Since(threadCPUStart) }

func pinThread(testing.TB) {}
