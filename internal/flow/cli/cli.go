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
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Usage is the usage of the commands this package serves.
const Usage = `usage: crw flow start WORKFLOW [--args JSON | --args-file FILE] [--thread T] [--token-budget N] [--pool N] [--decision D] [--preview] [--force]
       crw flow ps [--all] [--json]
       crw flow show RUN [--json]
       crw flow watch RUN [--after SEQ] [--wait N] [--limit M] [--follow] [--json]
       crw flow answer --decision D --run RUN --node N --question Q --revision V [--no-resume]
       crw flow pause RUN
       crw flow cancel RUN
       crw flow resume RUN [SCRIPT] [--detach]
       crw flow recover [--run RUN] CALL (--adopt THREAD|TURN | --absent)
       crw flow status-line --thread T
       crw flow digest RUN [--out FILE]
       crw flow audit (--run RUN | --acceptance ID) [--milestone M] [--json] [--no-record]
       crw flow apply --event E --turn T [--run RUN] [--base COMMIT] [--wait 10m] [--override REASON] [--manifest-request FILE] [--repo DIR]
       crw flow report-up --kind project_blocked|project_decision_request|jun_decision_request --run RUN --evidence FILE [--subject TEXT]
       crw flow reports [--initiative I] [--unread] [--json] [--project P]
       crw flow report-ack REPORT --turn T
       crw flow report-delivered --report ID --result accepted|queued|refused|unknown --request-id R [--observation O]
       (every command takes --state DIR and --socket PATH)`

// commands are the first words of the commands this package serves; resume and recover are served here for the run lock and the
// forms the engine's commands do not have, and hand the rest to the engine. audit is `crw flow audit` (audit.go); the reach-up
// commands of Commands (apply.go, report.go) run over an Environment (RunEnv).
var commands = map[string]bool{"start": true, "ps": true, "show": true, "watch": true, "answer": true, "pause": true, "cancel": true,
	"status-line": true, "digest": true, "resume": true, "recover": true, "audit": true, ProcessCommand: true,
	"apply": true, "report-up": true, "reports": true, "report-ack": true, "report-delivered": true}

// Commands are the `crw flow` commands that work on the relay store's flow rows over an Environment: apply, report-up, reports,
// report-ack and report-delivered reach up from a run (CRW-1204, F11), and audit (CRW-1206, F13, audit.go) reads the reviews behind
// the acceptances of a run back from the reviewer threads on the host.
var Commands = []string{"apply", "report-up", "reports", "report-ack", "report-delivered", "audit"}

// The exit statuses of the commands of Commands: 0 an application or a record that was made, 1 a refusal, 2 a command line or an
// infrastructure failure, 3 an application that waits for a later turn.
const (
	ExitOK       = 0
	ExitRefused  = 1
	ExitUsage    = 2
	ExitNextTurn = 3
)

// Handles reports whether `crw flow args...` is one of the commands this package serves.
func Handles(args []string) bool { return len(args) > 0 && commands[args[0]] }

// Run is `crw flow` for the commands of Handles, over the process's own environment.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return RunWith(ctx, Default(), args, stdin, stdout, stderr)
}

// RunWith is Run over explicit dependencies.
func RunWith(ctx context.Context, d *Deps, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 0 || !commands[args[0]] {
		fmt.Fprintln(stderr, Usage)
		return 2
	}
	if slices.Contains(Commands, args[0]) {
		// the reach-up commands open the relay store themselves; the audit opens its store and its host through AuditEnv, which the
		// binary installs (OpenAuditStore)
		return RunEnv(ctx, args, Environment{Stdout: stdout, Stderr: stderr})
	}
	c := &command{Deps: d, ctx: ctx, stdin: stdin, stdout: stdout, stderr: stderr, name: args[0]}
	switch args[0] {
	case "start":
		return c.start(args[1:])
	case "ps":
		return c.ps(args[1:])
	case "show":
		return c.show(args[1:])
	case "watch":
		return c.watch(args[1:])
	case "answer":
		return c.answer(args[1:])
	case "pause":
		return c.control(args[1:], "pause")
	case "cancel":
		return c.control(args[1:], "cancel")
	case "status-line":
		return c.statusLine(args[1:])
	case "digest":
		return c.digest(args[1:])
	case "resume":
		return c.resume(args[1:])
	case "recover":
		return c.recoverCall(args[1:])
	case ProcessCommand:
		return c.process(args[1:])
	}
	return 2
}

// command is one invocation.
type command struct {
	*Deps
	ctx            context.Context
	stdin          io.Reader
	stdout, stderr io.Writer
	name           string
}

func (c *command) fail(format string, a ...any) int {
	fmt.Fprintf(c.stderr, "crw flow %s: error: %s\n", c.name, fmt.Sprintf(format, a...))
	return 2
}

func (c *command) usage(format string, a ...any) int {
	fmt.Fprintln(c.stderr, Usage)
	return c.fail(format, a...)
}

// common are the flags every command takes.
type common struct {
	state, socket string
}

func (c *command) flags() (*flag.FlagSet, *common) {
	fs := flag.NewFlagSet("crw flow "+c.name, flag.ContinueOnError)
	fs.SetOutput(c.stderr)
	fs.Usage = func() { fmt.Fprintln(c.stderr, Usage) }
	cm := &common{}
	fs.StringVar(&cm.state, "state", "", "the relay state directory (default: the relay's own selection)")
	fs.StringVar(&cm.socket, "socket", "", "the App Server control socket (default: the default control socket)")
	return fs, cm
}

// parse reads a command line whose flags may follow the positional arguments. ok is false when the command has been answered
// (help, or a usage error): code is its exit status.
func parse(fs *flag.FlagSet, args []string) (positional []string, code int, ok bool) {
	rest := args
	for {
		if err := fs.Parse(rest); err != nil {
			if errors.Is(err, flag.ErrHelp) {
				return nil, 0, false
			}
			return nil, 2, false
		}
		if fs.NArg() == 0 {
			return positional, 0, true
		}
		positional = append(positional, fs.Arg(0))
		rest = fs.Args()[1:]
	}
}

func (c *command) reader(cm *common) (*Reader, error) {
	open := c.Reader
	if open == nil {
		open = OpenReader
	}
	return open(c.ctx, cm.state, cm.socket)
}

// since renders how long ago an RFC 3339 time was, for a table cell.
func (c *command) since(at string) string {
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return "-"
	}
	d := c.now().Sub(t).Round(time.Second)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return d.String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func (c *command) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *command) getenv(key string) string {
	if c.Getenv != nil {
		v, _ := c.Getenv(key)
		return v
	}
	return os.Getenv(key)
}

func short(id string) string {
	if len(id) > 14 {
		return id[:14]
	}
	return id
}

func cell(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return "-"
	}
	return s
}

// Environment is what the commands work through; the binary leaves it as it is and a test replaces what it needs.
type Environment struct {
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

// RunEnv is `crw flow <command>` for a command of Commands, over the environment given.
func RunEnv(ctx context.Context, args []string, env Environment) int {
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
	case "audit":
		return RunAudit(ctx, args, env.Stdout, env.Stderr)
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
func flags(name string, args []string, env Environment, define func(*flag.FlagSet)) (positional []string, code int, ok bool) {
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

func usageErr(env Environment, name, format string, a ...any) int {
	fmt.Fprintln(env.Stderr, Usage)
	fmt.Fprintf(env.Stderr, "crw flow %s: error: %s\n", name, fmt.Sprintf(format, a...))
	return ExitUsage
}

func failure(env Environment, name string, err error) int {
	fmt.Fprintf(env.Stderr, "crw flow %s: error: %s\n", name, strings.TrimSpace(err.Error()))
	return ExitUsage
}
