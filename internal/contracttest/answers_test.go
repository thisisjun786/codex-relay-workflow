package contracttest

import (
	"bytes"
	"errors"
	"os/exec"
	"strconv"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/golden"
)

// processAnswer is what one process answered: its exit status and output bytes.
type processAnswer struct {
	exit           int
	stdout, stderr []byte
}

// runProcess runs cmd to completion and returns its answer; only a failure to start is an error.
func runProcess(cmd *exec.Cmd) (processAnswer, error) {
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	answer := processAnswer{}
	if err := cmd.Run(); err != nil {
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			return answer, err
		}
		answer.exit = exit.ExitCode()
	}
	answer.stdout, answer.stderr = stdout.Bytes(), stderr.Bytes()
	return answer, nil
}

// checkProcess compares a process's answer with the goldens kept under key: its exit status and
// each output stream as bytes (internal/testsupport/golden).
func checkProcess(t *testing.T, key string, answer processAnswer, opts ...golden.Option) {
	t.Helper()
	golden.Check(t, key+" exit", []byte(strconv.Itoa(answer.exit)), opts...)
	golden.Check(t, key+" stdout", answer.stdout, opts...)
	golden.Check(t, key+" stderr", answer.stderr, opts...)
}
