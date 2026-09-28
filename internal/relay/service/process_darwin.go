//go:build darwin

package service

import (
	"golang.org/x/sys/unix"
	"os/exec"
	"time"
)

type ProcessHandle struct {
	PID, FD int
	Gone    bool
	Detail  string
}

func OpenProcess(pid int) *ProcessHandle {
	return &ProcessHandle{PID: pid, FD: -1, Detail: "this build has no pidfd support, so a signal cannot be aimed"}
}
func (h *ProcessHandle) Close() error            { return nil }
func (h *ProcessHandle) Send(unix.Signal) bool   { return false }
func (h *ProcessHandle) Wait(time.Duration) bool { return false }
func Monotonic() float64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(err)
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9
}
func workerAttributes(cmd *exec.Cmd) {}
