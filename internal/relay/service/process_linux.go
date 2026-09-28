//go:build linux

package service

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type ProcessHandle struct {
	PID, FD int
	Gone    bool
	Detail  string
}

func OpenProcess(pid int) *ProcessHandle {
	h := &ProcessHandle{PID: pid, FD: -1}
	fd, err := unix.PidfdOpen(pid, 0)
	if err == nil {
		h.FD = fd
	} else if errors.Is(err, unix.ESRCH) {
		h.Gone = true
		h.Detail = "the process is already gone"
	} else {
		h.Detail = fmt.Sprintf("OSError: %v", err)
	}
	return h
}
func (h *ProcessHandle) Close() error {
	if h.FD < 0 {
		return nil
	}
	err := unix.Close(h.FD)
	h.FD = -1
	return err
}
func (h *ProcessHandle) Send(sig unix.Signal) bool {
	if h.FD < 0 {
		return false
	}
	err := unix.PidfdSendSignal(h.FD, sig, nil, 0)
	if err != nil && !errors.Is(err, unix.ESRCH) {
		h.Detail = fmt.Sprintf("OSError: %v", err)
		return false
	}
	return true
}
func (h *ProcessHandle) Wait(timeout time.Duration) bool {
	if h.FD < 0 {
		return h.Gone
	}
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		fds := []unix.PollFd{{Fd: int32(h.FD), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, int((remaining+time.Millisecond-1)/time.Millisecond))
		if errors.Is(err, unix.EINTR) {
			continue
		}
		return err == nil && n > 0
	}
}
func Monotonic() float64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_MONOTONIC, &ts); err != nil {
		panic(err)
	}
	return float64(ts.Sec) + float64(ts.Nsec)/1e9
}
func workerAttributes(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
