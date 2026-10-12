package doctor

import (
	"flag"
	"fmt"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/bound"
)

// flowBoundCLI is `crw doctor flow-bound [--json]` (F14, CRW-1207): the check the native spawn hook makes of the flow-bound
// directory before it trusts any verdict under it, by the same function (bound.CheckDir). A directory that is not there is
// fine (no flow run has bound a thread yet); one that is a symlink, is not the current user's or is not mode 0700 makes the hook
// refuse every spawn of a thread-shaped session (flow_bound_unavailable, unsafe_dir), and this command exits 1 with the reason.
// It prints one JSON document and reads nothing but the directory's own status.
func flowBoundCLI(args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw doctor flow-bound", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Bool("json", true, "print the report as JSON (the only form)")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageExit
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw doctor flow-bound: unknown argument %q\n", flags.Arg(0))
		return usageExit
	}
	report := func(dir, state, detail string, exit int) int {
		var path, why any
		if dir != "" {
			path = dir
		}
		if detail != "" {
			why = detail
		}
		if err := contract.Emit(stdout, contract.OrderedObject{{Key: "directory", Value: path}, {Key: "state", Value: state}, {Key: "detail", Value: why}}); err != nil {
			return 1
		}
		return exit
	}
	dir, err := bound.Dir()
	if err != nil {
		return report("", "unknown", "the flow-bound directory cannot be located: "+err.Error(), 1)
	}
	exists, err := bound.CheckDir(dir)
	switch {
	case err != nil:
		return report(dir, bound.ReasonUnsafeDir, err.Error(), 1)
	case !exists:
		return report(dir, "absent", "", 0)
	}
	return report(dir, "safe", "", 0)
}
