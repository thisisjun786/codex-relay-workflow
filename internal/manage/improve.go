package manage

import (
	"context"
	"fmt"
)

// improveUsage is what the improve command prints.
const improveUsage = "usage: crw manage improve collect [--out FILE]\n" +
	"       crw manage improve propose --bundle FILE [--dry-run]\n" +
	"       crw manage improve run --boundary <milestone|project> --ref KEY"

// improveCommand is crw manage improve: the operating records of a management session,
// normalized into one improvement evidence bundle. It only proposes; it never releases,
// merges or writes to Linear.
var improveCommand = Command{Name: "improve", Summary: "read the operating records into one improvement evidence bundle", Run: improveRun}

func init() { Register(improveCommand) }

// improveRun is crw manage improve. The collect subcommand is the only one this issue
// adds; the help flags keep their own path so the usage stays reachable without it.
func improveRun(ctx context.Context, e *Env, args []string) int {
	if len(args) == 0 {
		fmt.Fprintln(e.Stderr, improveUsage)
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		fmt.Fprintln(e.Stdout, improveUsage)
		return 0
	case "collect":
		return improveRunCollect(ctx, e, args[1:])
	case "propose":
		return improveRunPropose(ctx, e, args[1:])
	case "run":
		return improveRunRoadmap(ctx, e, args[1:])
	}
	fmt.Fprintln(e.Stderr, improveUsage)
	fmt.Fprintf(e.Stderr, "crw manage improve: error: invalid command %q (choose from 'collect', 'propose', 'run')\n", args[0])
	return usageExit
}
