package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	// The merge-turn-* relay commands register themselves on the relay CLI.
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

var version = "dev"

// parserExit is argparse's exit status for a command line it cannot parse; the Python CLIs
// this binary replaces all exit 2 there, and 0 for -h/--help and the bridge's --version.
const parserExit = 2

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[0], os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, program string, args []string, stdout, stderr io.Writer) int {
	cli.Version = version
	switch filepath.Base(program) {
	case "codex-session-relay":
		return cli.ExecuteAs(ctx, program, args, stdout, stderr)
	case "codex-thread-bridge":
		return mcp.Run(ctx, args)
	case "crw-completion-hook":
		// The hook domain owns this entry point; nothing is dispatched until it is ported.
		usage(stderr)
		return parserExit
	}
	if len(args) == 0 {
		usage(stderr)
		fmt.Fprintln(stderr, "crw: error: the following arguments are required: command")
		return parserExit
	}
	switch mode, rest := args[0], args[1:]; mode {
	case "relay":
		return cli.ExecuteAs(ctx, "crw relay", rest, stdout, stderr)
	case "bridge":
		return mcp.Run(ctx, rest)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw: error: argument command: invalid choice: %s (choose from 'relay', 'bridge', 'help', 'version')\n", store.PythonRepr(mode))
		return parserExit
	}
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw [-h] [--version] {relay,bridge,help,version} ...")
}
