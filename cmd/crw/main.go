package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/pluginwiring"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	// The merge-turn-* relay commands register themselves on the relay CLI.
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/routing"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/sync"
)

var version = "dev"

// parserExit is argparse's exit status for a command line it cannot parse; the Python CLIs
// this binary replaces all exit 2 there, and 0 for -h/--help and the bridge's --version.
const parserExit = 2

func main() {
	started := time.Now()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(runAt(ctx, os.Args[0], os.Args[1:], os.Stdout, os.Stderr, started))
}

func run(ctx context.Context, program string, args []string, stdout, stderr io.Writer) int {
	return runAt(ctx, program, args, stdout, stderr, time.Now())
}

func runAt(ctx context.Context, program string, args []string, stdout, stderr io.Writer, started time.Time) int {
	cli.Version = version
	switch filepath.Base(program) {
	case "codex-session-relay":
		return relay(ctx, program, args, stdout, stderr)
	case "codex-thread-bridge":
		return mcp.Run(ctx, args)
	case "crw-completion-hook":
		return hook.Run(ctx, args, os.Stdin, stdout, started)
	}
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw: error: the following arguments are required: command")
		return parserExit
	}
	switch mode, rest := args[0], args[1:]; mode {
	case "relay":
		return relay(ctx, "crw relay", rest, stdout, stderr)
	case "bridge":
		if len(rest) > 0 && rest[0] == pluginwiring.Flag {
			return pluginwiring.Bridge(ctx, rest[1:])
		}
		return mcp.Run(ctx, rest)
	case "hook":
		return hook.Run(ctx, rest, os.Stdin, stdout, started)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw: error: argument command: invalid choice: %s (choose from 'relay', 'bridge', 'hook', 'help', 'version')\n", store.PythonRepr(mode))
		return parserExit
	}
}

// relay is the relay CLI. The capacity and edit-region commands (todo 27) parse their own line
// first, as argparse would, and are then dispatched through the relay CLI's command list.
func relay(ctx context.Context, program string, args []string, stdout, stderr io.Writer) int {
	prog := filepath.Base(program)
	if program == "crw relay" {
		prog = program
	}
	if code, handled := capacity.Precheck(prog, args, stdout, stderr); handled {
		return code
	}
	return cli.ExecuteAs(ctx, program, args, stdout, stderr)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw [-h] [--version] {relay,bridge,hook,help,version} ...")
}
