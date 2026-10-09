//go:build linux

package fdsweep

import (
	"math"

	"golang.org/x/sys/unix"
)

func closeRangeCloExec() error {
	return unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)
}
