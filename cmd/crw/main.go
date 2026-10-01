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

// parserExit is the exit status for a command line that cannot be parsed; -h/--help and the
// bridge's --version exit 0.
const parserExit = 2

func main() {
	started := time.Now()
	adapter.Register()
	os.Exit(serve(os.Args[0], os.Args[1:], os.Stdout, os.Stderr, started))
}

// serve is this process's whole run, from its SIGINT policy to the status it exits with: main is
// its only caller and exits with what it returns, so a registration serve does not release is
// held until the process ends. (A test that wants a command's answer runs run or runAt, which
// register no SIGINT handling.)
//
// The first SIGINT cancels the run, and it is registered before the command line is read, as it
// always was. Every line but a supervised worker's then gets SIGINT's default disposition back
// (releaseAfterFirst): a second interrupt ends a process whose first is being honoured slowly. A
// supervised worker is different (decision 42). One interrupt can reach it twice, by its process
// group or a drain and again by its supervisor passing the interrupt on, and a default-handled
// second copy would kill it before its cleanup, or after the run returned and before its exit
// status was reported, which the supervisor then records as -1. So its registration is never
// released: the catch stays until the process exits, and its hard stops stay SIGTERM, SIGKILL and
// its supervisor's death (PR_SET_PDEATHSIG). An interrupt that lands before main, in the Go
// runtime's start, meets the default disposition, as it always did.
func serve(program string, args []string, stdout, stderr io.Writer, started time.Time) int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	if !supervisedWorker(program, args) {
		releaseAfterFirst(ctx, stop)
	}
	return runAt(ctx, program, args, stdout, stderr, started)
}

// workerLine is cli.SupervisedWorker, which a test replaces to act while serve decides.
var workerLine = cli.SupervisedWorker

// supervisedWorker reports whether this invocation is the relay CLI given a supervised worker's
// line, reading which program it is as runAt does: by its name, or by its first word.
func supervisedWorker(program string, args []string) bool {
	switch filepath.Base(program) {
	case "codex-session-relay":
		return workerLine(args)
	case "codex-thread-bridge":
		return false
	}
	return len(args) > 0 && args[0] == "relay" && workerLine(args[1:])
}

// cancelOn is a context the first of signals cancels. The signals are handled only until then
// (releaseAfterFirst).
func cancelOn(parent context.Context, signals ...os.Signal) (context.Context, context.CancelFunc) {
	ctx, stop := signal.NotifyContext(parent, signals...)
	releaseAfterFirst(ctx, stop)
	return ctx, stop
}

// releaseAfterFirst gives the signals their default disposition once ctx is done, which the first
// of them brings about: a second one then ends the process, so an operator whose interrupt is
// being honoured slowly can still end it at once. It starts that wait and returns.
func releaseAfterFirst(ctx context.Context, stop context.CancelFunc) {
	go func() {
		<-ctx.Done()
		stop()
	}()
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
		fmt.Fprintf(stderr, "crw: error: argument command: invalid choice: %q (choose from 'relay', 'bridge', 'hook', 'skill', 'doctor', 'install', 'help', 'version')\n", mode)
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
