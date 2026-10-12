package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
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
       (every command takes --state DIR and --socket PATH)`

// commands are the first words of the commands this package serves; resume and recover are served here for the run lock and the
// forms the engine's commands do not have, and hand the rest to the engine.
var commands = map[string]bool{"start": true, "ps": true, "show": true, "watch": true, "answer": true, "pause": true, "cancel": true,
	"status-line": true, "digest": true, "resume": true, "recover": true, ProcessCommand: true}

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
