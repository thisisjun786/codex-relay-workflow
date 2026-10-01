package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/mcp"
	"github.com/thisisjun786/codex-relay-workflow/internal/pluginwiring"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/adapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/cli"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/doctor"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/install"
	"github.com/thisisjun786/codex-relay-workflow/internal/skill"

	// The relay commands register in the relay command table when their packages load; cli
	// brings its own and the registry, delivery and fault families.
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/managed"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/routing"
	_ "github.com/thisisjun786/codex-relay-workflow/internal/relay/sync"
)

var version = "dev"

// parserExit is argparse's exit status for a command line it cannot parse; the Python CLIs
// this binary replaces all exit 2 there, and 0 for -h/--help and the bridge's --version.
const parserExit = 2

func main() {
	started := time.Now()
	adapter.Register()
	ctx, stop := cancelOn(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(runAt(ctx, os.Args[0], os.Args[1:], os.Stdout, os.Stderr, started))
}

// cancelOn is a context the first of signals cancels. The signals are handled only until then:
// a second one gets its default disposition, so an operator whose interrupt is being honoured
// slowly can still end the process at once.
func cancelOn(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, signals...)
	go func() {
		<-ctx.Done()
		stop()
	}()
	return ctx, stop
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
		return bridge(ctx, program, args)
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
		return bridge(ctx, program, rest)
	case "hook":
		return hook.Run(ctx, rest, os.Stdin, stdout, started)
	case "skill":
		return skill.Run(rest, os.Stdin, stdout, stderr)
	case "doctor":
		return doctor.Run(ctx, rest, stdout, stderr)
	case "install":
		// An install command waits on locks and then removes or replaces things, so every way an
		// operator or a supervisor asks it to stop (SIGINT, SIGTERM, SIGHUP) cancels it: a wait
		// ends and nothing destructive follows. Other modes keep their own signal handling.
		ctx, stop := cancelOn(ctx, syscall.SIGTERM, syscall.SIGHUP)
		defer stop()
		return install.Run(ctx, rest, stdout, stderr)
	case "help", "-h", "--help":
		usage(stdout)
		return 0
	case "version", "--version":
		fmt.Fprintln(stdout, version)
		return 0
	default:
		usage(stderr)
		fmt.Fprintf(stderr, "crw: error: argument command: invalid choice: %s (choose from 'relay', 'bridge', 'hook', 'skill', 'doctor', 'install', 'help', 'version')\n", pyvalue.StrRepr(mode))
		return parserExit
	}
}

// bridge is the MCP bridge, or with the plugin's flag first (wiring/crw-bridge.sh execs
// `codex-thread-bridge --plugin-launch`) the launcher that reads the bridge record and then
// execs this binary as the bridge under it (decision 26).
func bridge(ctx context.Context, program string, args []string) int {
	if len(args) > 0 && args[0] == pluginwiring.Flag {
		return pluginwiring.Bridge(program, args[1:])
	}
	return mcp.Run(ctx, args)
}

// relay is the relay CLI.
func relay(ctx context.Context, program string, args []string, stdout, stderr io.Writer) int {
	return cli.ExecuteAs(ctx, program, args, stdout, stderr)
}

func usage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw [-h] [--version] {relay,bridge,hook,skill,doctor,install,help,version} ...")
}
