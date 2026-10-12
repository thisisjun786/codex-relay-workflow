// Package cli is the part of `crw flow` that works on the relay store's flow rows and reaches up from a run (CRW-1204, F11): apply,
// report-up, reports, report-ack and report-delivered. The engine's own commands (run, resume, recover, compile, roles) and lint stay
// where they are; the binary asks Handles first.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Commands are the `crw flow` commands this package runs.
var Commands = []string{"apply", "report-up", "reports", "report-ack", "report-delivered"}

// Usage is the usage of those commands.
const Usage = `usage: crw flow apply --event E --turn T [--run RUN] [--base COMMIT] [--wait 10m] [--override REASON] [--manifest-request FILE] [--repo DIR] [--state DIR] [--socket PATH]
       crw flow report-up --kind project_blocked|project_decision_request|jun_decision_request --run RUN --evidence FILE [--subject TEXT] [--state DIR] [--socket PATH]
       crw flow reports [--initiative I] [--unread] [--json] [--state DIR] [--socket PATH]
       crw flow report-ack REPORT --turn T [--state DIR] [--socket PATH]
       crw flow report-delivered --report ID --result accepted|queued|refused|unknown --request-id R [--observation O] [--state DIR] [--socket PATH]`

// The exit statuses: 0 an application or a record that was made, 1 a refusal, 2 a command line or an infrastructure failure, 3 an
// application that waits for a later turn.
const (
	ExitOK       = 0
	ExitRefused  = 1
	ExitUsage    = 2
	ExitNextTurn = 3
)

// Handles reports whether the first argument names a command of this package.
func Handles(args []string) bool { return len(args) > 0 && slices.Contains(Commands, args[0]) }

// Env is what the commands work through; the binary leaves it as it is and a test replaces what it needs.
type Env struct {
	// Stdout and Stderr are the command's streams.
	Stdout, Stderr io.Writer
	// Program is the crw executable relay commands are run with ("" is the running one).
	Program string
	// Open opens the relay store: the state directory and socket the operator named, else the relay's own selection.
	Open func(ctx context.Context, state, socket string) (*store.Store, string, error)
	// Getenv reads the environment.
	Getenv func(string) string
	// Sender and Resolver replace the notices through `crw manage send-parent` and the linkage walk (tests).
	Sender   report.Sender
	Resolver report.Resolver
}

// Run is `crw flow <command>` for a command of Commands.
func Run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	return RunEnv(ctx, args, Env{Stdout: stdout, Stderr: stderr})
}

// RunEnv is Run with its environment given.
func RunEnv(ctx context.Context, args []string, env Env) int {
	if env.Open == nil {
		env.Open = openStore
	}
	if env.Getenv == nil {
		env.Getenv = os.Getenv
	}
	if env.Program == "" {
		if exe, err := os.Executable(); err == nil {
			env.Program = exe
		}
	}
	if len(args) == 0 {
		fmt.Fprintln(env.Stderr, Usage)
		return ExitUsage
	}
	switch args[0] {
	case "apply":
		return applyCmd(ctx, args[1:], env)
	case "report-up":
		return reportUpCmd(ctx, args[1:], env)
	case "reports":
		return reportsCmd(ctx, args[1:], env)
	case "report-ack":
		return reportAckCmd(ctx, args[1:], env)
	case "report-delivered":
		return reportDeliveredCmd(ctx, args[1:], env)
	}
	fmt.Fprintln(env.Stderr, Usage)
	return ExitUsage
}

// openStore opens the relay store for writing: --state, else the relay's own selection for the socket. A store that does not exist is an
// error, never a new empty store.
func openStore(ctx context.Context, state, socket string) (*store.Store, string, error) {
	if socket == "" {
		var err error
		if socket, err = store.DefaultSocket(); err != nil {
			return nil, "", err
		}
	}
	selection, err := store.ResolveStateDir(state, socket)
	if err != nil {
		return nil, "", err
	}
	db := selection.DBPath()
	if _, err := os.Stat(db); err != nil {
		return nil, "", fmt.Errorf("there is no relay store at %s (%s): %w", db, selection.Detail, err)
	}
	st, err := store.Open(ctx, db, socket)
	return st, selection.Path, err
}

// flags parses a command's flags, which may follow its positional arguments.
func flags(name string, args []string, env Env, define func(*flag.FlagSet)) (positional []string, code int, ok bool) {
	fs := flag.NewFlagSet("crw flow "+name, flag.ContinueOnError)
	fs.SetOutput(env.Stderr)
	fs.Usage = func() { fmt.Fprintln(env.Stderr, Usage) }
	define(fs)
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, ExitOK, false
			}
			return nil, ExitUsage, false
		}
		if fs.NArg() == 0 {
			break
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
	return positional, ExitOK, true
}

func usageErr(env Env, name, format string, a ...any) int {
	fmt.Fprintln(env.Stderr, Usage)
	fmt.Fprintf(env.Stderr, "crw flow %s: error: %s\n", name, fmt.Sprintf(format, a...))
	return ExitUsage
}

func failure(env Env, name string, err error) int {
	fmt.Fprintf(env.Stderr, "crw flow %s: error: %s\n", name, strings.TrimSpace(err.Error()))
	return ExitUsage
}
