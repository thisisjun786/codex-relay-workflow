package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
)

var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(run(ctx, os.Args[0], os.Args[1:]))
}

func run(ctx context.Context, program string, args []string) int {
	var mode string
	switch filepath.Base(program) {
	case "codex-session-relay":
		mode = "relay"
	case "codex-thread-bridge":
		mode = "bridge"
	case "crw-completion-hook":
		mode = "hook"
	default:
		if len(args) == 0 {
			usage()
			return contract.ExitUsage
		}
		mode, args = args[0], args[1:]
	}
	if mode == "relay" && len(args) == 1 && args[0] == "--version" {
		if err := contract.Emit(os.Stdout, contract.Result{{Key: "version", Value: version}}); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return contract.ExitHost
		}
		return contract.ExitOk
	}
	switch mode {
	case "relay":
		return cli.Execute(ctx, args, os.Stdout, os.Stderr)
	case "bridge":
		return mcp.Run(ctx, args)
	case "hook", "install", "doctor", "version", "skill":
		// Only implemented commands are dispatched; future domains own these modes.
		usage()
		return contract.ExitUsage
	default:
		usage()
		return contract.ExitUsage
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: crw {relay|bridge|hook|install|doctor|version|skill} [args]")
}
