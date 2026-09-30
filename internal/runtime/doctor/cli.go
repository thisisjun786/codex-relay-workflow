package doctor

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/swapgate"
)

// usageExit is the exit status of a command line this command cannot parse.
const usageExit = 2

// Run is `crw doctor [retention-scan | declared-schema] ...`. Every form prints one JSON
// document (json.dumps(indent=2) with ensure_ascii, as the Python diagnosis emitted) and exits
// 0 once it has something to report; --json is accepted and is the only output there is.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	env := scope.Env(os.Environ())
	if len(args) > 0 {
		switch args[0] {
		case "retention-scan":
			return retentionScan(ctx, env, args[1:], stdout, stderr)
		case "declared-schema":
			return declaredSchema(ctx, args[1:], stdout, stderr)
		}
	}
	flags := flag.NewFlagSet("crw doctor", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Bool("json", true, "print the report as JSON (the only form)")
	codexHome := flags.String("codex-home", "", "the Codex home (default $CODEX_HOME or ~/.codex)")
	recordPath := flags.String("record", "", "the host record (default $XDG_STATE_HOME/codex-relay-workflow/host-record.json)")
	destination := flags.String("dest", "", "the runtime destination (default: the recorded pointer, else ~/.local/share/crw-runtime)")
	relayCommand := flags.String("relay-command", "", "the relay executable to read the scope from (default: current/bin/codex-session-relay)")
	socket := flags.String("socket", "", "the App Server socket to pass to the relay")
	state := flags.String("state", "", "the relay state directory to pass to the relay")
	temporary := flags.Bool("temporary", false, "record the destination as a temporary one")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageExit
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw doctor: unknown argument %q (choose from retention-scan, declared-schema)\n", flags.Arg(0))
		return usageExit
	}
	options := Options{Env: env, CodexHome: *codexHome, RecordPath: *recordPath, RelayCommand: *relayCommand, Socket: *socket, State: *state, Temporary: *temporary}
	flags.Visit(func(f *flag.Flag) {
		if f.Name == "dest" {
			options.Destination = destination
		}
	})
	return emit(stdout, Diagnose(ctx, options))
}

func retentionScan(ctx context.Context, env scope.Env, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw doctor retention-scan", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Bool("json", true, "print the report as JSON (the only form)")
	codexHome := flags.String("codex-home", "", "the Codex home (default $CODEX_HOME or ~/.codex)")
	destination := flags.String("dest", "", "the runtime destination (default ~/.local/share/crw-runtime)")
	stateRoot := flags.String("state-root", "", "the relay state root (default $XDG_STATE_HOME/codex-session-relay)")
	socket := flags.String("socket", "", "the App Server socket row 7 lists threads through (default: crw bridge's, <codex home>/app-server-control/app-server-control.sock)")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageExit
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw doctor retention-scan: unknown argument %q\n", flags.Arg(0))
		return usageExit
	}
	return emit(stdout, RetentionScan(ctx, RetentionOptions{Env: env, CodexHome: *codexHome, Destination: *destination, StateRoot: *stateRoot, Socket: *socket}))
}

// declaredSchema is what a candidate binary answers the swap gate with: the schema objects
// this build's store declares, applied to an in-memory database.
func declaredSchema(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw doctor declared-schema", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.Bool("json", true, "print the answer as JSON (the only form)")
	if err := flags.Parse(args); err != nil {
		if err == flag.ErrHelp {
			return 0
		}
		return usageExit
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "crw doctor declared-schema: unknown argument %q\n", flags.Arg(0))
		return usageExit
	}
	return emit(stdout, swapgate.DeclaredSchema(ctx))
}

func emit(w io.Writer, value contract.OrderedObject) int {
	if err := contract.Emit(w, value); err != nil {
		return 1
	}
	return 0
}
