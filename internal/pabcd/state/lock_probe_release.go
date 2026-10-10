//go:build !sigintprobe

package state

import "time"

// lockWaitProbe is the lock wait's schedule and busy observer for a test that has to send a signal while a
// built crw waits for the session lock. A release build has none (nil, nil: the oracle's schedule, no
// observer); lock_probe.go, built only with the sigintprobe tag, supplies them. Production never reads a
// test environment (CRW-1167).
func lockWaitProbe() ([]time.Duration, func()) { return nil, nil }
