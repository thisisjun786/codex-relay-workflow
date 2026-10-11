package integrator

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Cmd is one program the integrator runs.
type Cmd struct {
	Argv   []string
	Dir    string
	Env    []string // nil is the process environment
	Stdout io.Writer
	Stderr io.Writer
}

// Exec runs a program and reports its exit status. The error is set when the program could
// not be started or was killed by a signal (the status is then -1); a program that ran and
// exited non-zero is a status with no error.
type Exec func(ctx context.Context, c Cmd) (int, error)

// OSExec runs the program as a child in its own process group, so cancelling ctx ends the
// program and everything it started.
func OSExec(ctx context.Context, c Cmd) (int, error) {
	if len(c.Argv) == 0 {
		return -1, errors.New("empty command")
	}
	cmd := exec.CommandContext(ctx, c.Argv[0], c.Argv[1:]...)
	cmd.Dir = c.Dir
	cmd.Env = c.Env
	cmd.Stdout = c.Stdout
	cmd.Stderr = c.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	err := cmd.Run()
	if err == nil {
		return 0, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		if code := exit.ExitCode(); code >= 0 {
			return code, nil
		}
		return -1, err
	}
	return -1, err
}

// processEnv is the environment programs run under: the process's own.
func processEnv() []string { return os.Environ() }
