package main

import (
	"strconv"
	"time"
)

// recallTestClock is the recall CLI's optional link-time clock, decimal Unix milliseconds. Nothing
// in this repository initializes it: a release build leaves it empty and the mode runs on the wall
// clock. Only a test build links a value in (-X main.recallTestClock=<ms>), which the cxc domain of
// internal/contracttest does so the nine clock-dependent corpus fixtures replay under the instant
// the oracle recorded with (2026-01-01T00:00:00.000Z, contract/notes/cxc/README.md "Seams the
// replay does not provide").
var recallTestClock string

// recallNow is the clock the recall mode runs with: the wall clock when no seam is linked, else
// the frozen instant in UTC. A malformed value means the build was linked wrongly, so it panics
// with the value it read instead of running with a moving clock.
func recallNow() time.Time {
	if recallTestClock == "" {
		return time.Now()
	}
	ms, err := strconv.ParseInt(recallTestClock, 10, 64)
	if err != nil {
		panic("recallTestClock " + strconv.Quote(recallTestClock) + ": " + err.Error())
	}
	return time.UnixMilli(ms).UTC()
}
