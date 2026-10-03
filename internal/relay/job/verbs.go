package job

import (
	"io"
	"time"
)

type CLIResult struct {
	Out  any
	Code int
}

const MaxCLIStdinBytes = 1024 * 1024

func ReadCLIStdin(io.Reader) string { return "" }
func ParseCLIPayload(string) any    { return map[string]any{} }
func RunCLI([]string, string, func(string) (string, bool), func() time.Time) (CLIResult, error) {
	return CLIResult{Out: ""}, nil
}
