package evidence

import (
	"context"
	"os/exec"
	"strings"
	"time"
)

// ExecRunner is the process runner the forge reads through in production: it runs the argv it is given under the context and the timeout, and reports the exit code and both streams. A command
// that ran and failed is an exit code with a nil error; one that could not be started, or that the deadline or the context ended, is an error.
func ExecRunner(ctx context.Context) Runner {
	return func(argv []string, timeout time.Duration) (int, string, string, error) {
		runCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
		var stdout, stderr strings.Builder
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		if runCtx.Err() != nil {
			return 0, "", "", runCtx.Err()
		}
		if err == nil {
			return 0, stdout.String(), stderr.String(), nil
		}
		if exit, ok := err.(*exec.ExitError); ok {
			return exit.ExitCode(), stdout.String(), stderr.String(), nil
		}
		return 0, stdout.String(), stderr.String(), err
	}
}
