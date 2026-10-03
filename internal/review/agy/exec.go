// Adapted from agentic-code-reviewer (https://github.com/richhaase/agentic-code-reviewer),
// internal/agent/executor.go, internal/agent/cmd_reader.go and internal/agent/result.go at commit a3e438e2bd1f0824c1eab88db738aa3c82c69e99,
// licensed under the Apache License 2.0 (see docs/port-acr/LICENSE). Modified for CRW.

package agy

import (
	"bytes"
	"context"
	"os/exec"
	"sync/atomic"
	"time"
)

// killGrace is how long Wait lasts, after the process ended or was killed, for a descendant that still holds stdout or stderr open.
const killGrace = 5 * time.Second

// cappedBuffer keeps the first max bytes written to it and drops the rest, reporting every write as complete so the writer is never stopped (ACR's cappedBuffer).
type cappedBuffer struct {
	buf  bytes.Buffer
	max  int
	over bool
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := c.max - c.buf.Len(); n > room {
		c.over = true
		p = p[:max(room, 0)]
	}
	c.buf.Write(p)
	return n, nil
}

// execution is what one agy process did.
type execution struct {
	stdout, stderr []byte
	truncated      bool // stdout or stderr went over the cap
	exitCode       int  // -1 when the process was killed by a signal or did not start
	startErr       error
	timedOut       bool // the time limit killed the process group
	limit          time.Duration
	elapsed        time.Duration
}

// execute runs bin in dir with the prompt on stdin, in its own process group, and ends that group when the limit passes or ctx is done (ACR's executeCommand
// and cmdReader.Close, with the output read to the end under a cap instead of streamed).
func execute(ctx context.Context, limit time.Duration, bin string, args []string, dir string, env []string, stdin []byte, maxOut int) execution {
	tctx, cancel := context.WithTimeout(ctx, limit)
	defer cancel()
	cmd := exec.CommandContext(tctx, bin, args...)
	cmd.Dir, cmd.Env, cmd.Stdin = dir, env, bytes.NewReader(stdin)
	stdout, stderr := &cappedBuffer{max: maxOut}, &cappedBuffer{max: maxOut}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	configureProcessGroup(cmd)
	var killed atomic.Bool
	cmd.Cancel = func() error {
		killed.Store(true)
		return terminateProcessGroup(cmd)
	}
	cmd.WaitDelay = killGrace
	start := time.Now()
	err := cmd.Run()
	ex := execution{stdout: stdout.buf.Bytes(), stderr: stderr.buf.Bytes(), truncated: stdout.over || stderr.over, exitCode: -1, limit: limit, elapsed: time.Since(start)}
	if cmd.ProcessState == nil {
		ex.startErr = err
		return ex
	}
	ex.exitCode = cmd.ProcessState.ExitCode()
	ex.timedOut = killed.Load() && ctx.Err() == nil
	return ex
}
