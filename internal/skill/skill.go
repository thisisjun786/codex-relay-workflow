// Package skill implements the executable helpers shipped with CRW skills.
package skill

import (
	"fmt"
	"io"
)

// Run dispatches `crw skill` commands.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "usage: crw skill [-h] {hook-probe,parent-title,start-policy} ...")
		fmt.Fprintln(stderr, "crw skill: error: the following arguments are required: command")
		return 2
	}
	if parsed, code, handled := precheckPythonArgs(args, stdout, stderr); handled {
		return code
	} else {
		args = parsed
	}
	switch args[0] {
	case "hook-probe":
		return runHookProbe(args[1:], stdout, stderr)
	case "parent-title":
		return runParentTitle(args[1:], stdin, stdout, stderr)
	case "start-policy":
		return runStartPolicy(args[1:], stdin, stdout, stderr)
	case "-h", "--help":
		fmt.Fprintln(stdout, "usage: crw skill [-h] {hook-probe,parent-title,start-policy} ...")
		return 0
	default:
		fmt.Fprintf(stderr, "crw skill: error: argument command: invalid choice: %q (choose from 'hook-probe', 'parent-title', 'start-policy')\n", args[0])
		return 2
	}
}
