// Package skill implements the executable helpers shipped with CRW skills.
package skill

import (
	"fmt"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/skill/search"
)

const skillUsage = "usage: crw skill {hook-probe,issue-size,parent-title,start-policy,base-refresh} ..."

// Run dispatches `crw skill` commands.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, skillUsage)
		fmt.Fprintln(stderr, "crw skill: error: a command is required")
		return usageExit
	}
	switch args[0] {
	case "search", "show":
		return search.Run(args, nil, stdout, stderr)
	case "hook-probe":
		return runHookProbe(args[1:], stdout, stderr)
	case "pair-choice":
		return runPairChoice(args[1:], stdin, stdout, stderr)
	case "issue-size":
		return runIssueSize(args[1:], stdin, stdout, stderr)
	case "parent-title":
		return runParentTitle(args[1:], stdin, stdout, stderr)
	case "start-policy":
		return runStartPolicy(args[1:], stdin, stdout, stderr)
	case "base-refresh":
		return runBaseRefresh(args[1:], stdout, stderr)
	case "-h", "--help":
		fmt.Fprintln(stdout, skillUsage)
		fmt.Fprintln(stdout, search.Usage)
		return 0
	default:
		fmt.Fprintln(stderr, skillUsage)
		fmt.Fprintf(stderr, "crw skill: error: invalid command %q (choose from hook-probe, issue-size, parent-title, start-policy, base-refresh)\n", args[0])
		return usageExit
	}
}
