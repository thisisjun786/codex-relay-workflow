//go:build sigintprobe

package state

import (
	"os"
	"time"
)

// sigintProbeReadyEnv names the file a crw built with -tags sigintprobe creates when its session lock wait first
// finds the lock held. The file is the test's observable "the process has installed its SIGINT handler and is
// blocked on the lock": the wait runs inside the row, long after serve registered the handler (CRW-1167).
const sigintProbeReadyEnv = "CRW_SIGINTPROBE_READY_FILE"

// lockWaitProbe replaces the 250 ms wait with one the test cannot outrun (30 s, ended only by the invocation's
// context, a release of the lock, or the test's own timeout) and reports the first busy attempt through the
// ready file. Without the variable it is the release build's.
func lockWaitProbe() ([]time.Duration, func()) {
	ready := os.Getenv(sigintProbeReadyEnv)
	if ready == "" {
		return nil, nil
	}
	delays := make([]time.Duration, 600)
	for i := range delays {
		delays[i] = 50
	}
	return delays, func() {
		if f, err := os.OpenFile(ready, os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			_ = f.Close()
		}
	}
}
