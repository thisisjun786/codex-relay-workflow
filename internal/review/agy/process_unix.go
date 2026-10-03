// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/agent/process_unix.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package agy

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// groupGrace is the longest a call waits, after killing its process group, for the members to be gone.
const groupGrace = 2 * time.Second

// processGroup is the process group of one call. Its leader is a sentinel, a shell that waits on a pipe this runner holds and that nobody reaps until close,
// so the group id cannot be taken by another process while any signal may still be sent to it, whatever agy and its descendants do. agy joins the group
// and stays a child of this runner, so its exit status is its own.
type processGroup struct {
	sentinel *exec.Cmd
	hold     io.WriteCloser // the sentinel's stdin: it also exits if this runner dies
	pgid     int
}

func startProcessGroup() (*processGroup, error) {
	s := exec.Command("/bin/sh", "-c", "read _")
	s.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	hold, err := s.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := s.Start(); err != nil {
		return nil, err
	}
	return &processGroup{sentinel: s, hold: hold, pgid: s.Process.Pid}, nil
}

// join makes cmd start inside the group.
func (g *processGroup) join(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: g.pgid}
}

// kill ends every member of the group (ACR's terminateProcessGroup: SIGKILL to the negative group id, ESRCH read as done).
func (g *processGroup) kill() error {
	err := syscall.Kill(-g.pgid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// close kills the group, reaps the sentinel, and reports whether the group has no member left within groupGrace. A killed member nobody has reaped yet still
// counts as one; init reaps it in moments. After close the group id is free.
func (g *processGroup) close() bool {
	_ = g.kill()
	_ = g.hold.Close()
	_ = g.sentinel.Wait()
	for deadline := time.Now().Add(groupGrace); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if errors.Is(syscall.Kill(-g.pgid, 0), syscall.ESRCH) {
			return true
		}
	}
	return false
}
