package harness

import (
	"fmt"
	"io"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/reset"
)

const resetUsage = "usage: crw pabcd reset [-h] [--state | --generated | --goalplans | --all]"

func resetVerb(args []string, _ io.Reader, stdout, stderr io.Writer) int {
	for _, arg := range args {
		if arg == "-h" || arg == "--help" {
			fmt.Fprintln(stdout, resetUsage)
			return 0
		}
	}
	scope, err := reset.ParseResetScope(args)
	if err != nil {
		fmt.Fprintln(stderr, "reset: "+err.Error()+"\n"+resetUsage)
		return 2
	}
	cwd, err := syscall.Getwd()
	if err == nil {
		var result reset.ResetResult
		result, err = reset.RunReset(cwd, scope)
		if err == nil {
			fmt.Fprintln(stdout, reset.RenderReset(result))
			return 0
		}
	}
	fmt.Fprintln(stderr, "crw cli failed: "+err.Error())
	return 1
}
