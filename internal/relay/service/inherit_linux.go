//go:build linux

package service

import (
	"math"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

func closeRangeCloExec() error {
	return unix.CloseRange(3, math.MaxUint32, unix.CLOSE_RANGE_CLOEXEC)
}

// kernelMaxDescriptors is fs.nr_open, the number that no descriptor of any process reaches.
func kernelMaxDescriptors() (uint64, error) {
	raw, err := os.ReadFile("/proc/sys/fs/nr_open")
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(raw)), 10, 64)
}
