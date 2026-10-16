// Package execgroup ends a cancelled command together with everything it started (CRW-1573). exec.CommandContext alone kills
// only the first process at a cancel: a child it started (a test binary under `go test`, a background job of a shell) keeps
// running and keeps the command's output pipes open, so Wait blocks until that child ends on its own. Bind puts the command in
// its own process group, kills the whole group at the cancel and bounds the wait for the pipes, as the review adapter's runner
// does (flow/reviewadapter ExecRun). crw is released for Linux and macOS only (host.ShellWord), where process groups exist.
package execgroup

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// KillGrace is how long Wait still collects the output after the group is killed, or after the command exits on its own while
// a process outside its control holds the pipes; then exec closes them and Wait returns.
const KillGrace = 5 * time.Second

// Bind makes cmd, made by exec.CommandContext and not yet started, the leader of a new process group: when the context is done the
// whole group gets SIGKILL, and Wait waits at most grace for the output after that (cmd.WaitDelay). Attributes the caller already
// set in cmd.SysProcAttr are kept but for the group (Setpgid, Pgid). A group that is already gone is the finished process, not a cancel error.
func Bind(cmd *exec.Cmd, grace time.Duration) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.SysProcAttr.Pgid = 0
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = grace
}
