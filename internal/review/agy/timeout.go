// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/review/timeout.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package agy

import "time"

// sizeScaleThreshold is the prompt size up to which the call time limit stays at its floor (ACR's reviewerTimeoutScaleThreshold).
const sizeScaleThreshold = 100 * 1024

// timeLimit is the call time limit for a prompt of promptBytes: floor up to 100 KiB, then in proportion to the size, as ACR scales its reviewer timeout by
// the diff size; never below floor and never above ceiling (a ceiling under the floor cannot lower it).
func timeLimit(floor, ceiling time.Duration, promptBytes int) time.Duration {
	ceiling = max(ceiling, floor)
	if promptBytes <= sizeScaleThreshold {
		return floor
	}
	scaled := float64(floor) * (float64(promptBytes) / sizeScaleThreshold)
	if scaled >= float64(ceiling) {
		return ceiling
	}
	return time.Duration(scaled)
}
