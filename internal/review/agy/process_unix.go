// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/agent/process_unix.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package agy

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// groupGrace is the longest a call waits, after killing the process group, for its members to be gone.
const groupGrace = 2 * time.Second

// configureProcessGroup starts the process as the leader of a new process group, so everything it starts can be ended through that group.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessGroup kills the group this runner created for cmd, and nothing else: the group id is the pid of the process it started.
func terminateProcessGroup(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil || cmd.Process.Pid <= 0 {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// waitProcessGroupGone polls until the process group has no member left (a killed member nobody has reaped yet still counts) or until grace has passed.
func waitProcessGroupGone(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	for deadline := time.Now().Add(grace); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if errors.Is(syscall.Kill(-cmd.Process.Pid, 0), syscall.ESRCH) {
			return
		}
	}
}
