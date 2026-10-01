package skill

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
)

// The `crw skill` command line: a family (hook-probe, parent-title, start-policy), one of its
// commands, then the command's flags and positional arguments, read with the flag package. -h
// or --help prints the usage on stdout and exits 0; a usage error prints it on stderr and exits 2.

const usageExit = 2

// family is one `crw skill <family>`: what it is for and its commands, in the order its usage
// lists them.
type family struct {
	name, description string
	commands          [][2]string // name, summary
}

func (f family) usage(w io.Writer) {
	fmt.Fprintf(w, "usage: crw skill %s <command> [flags]\n\n%s\n\ncommands:\n", f.name, f.description)
	for _, c := range f.commands {
		fmt.Fprintf(w, "  %-10s  %s\n", c[0], c[1])
	}
}

// command is the command args name: ok is false when the family's own usage was printed, or a
// usage error, and code is then the exit status.
func (f family) command(args []string, stdout, stderr io.Writer) (name string, code int, ok bool) {
	switch {
	case len(args) == 0:
		f.usage(stderr)
		fmt.Fprintf(stderr, "crw skill %s: error: a command is required\n", f.name)
		return "", usageExit, false
	case args[0] == "-h" || args[0] == "--help":
		f.usage(stdout)
		return "", 0, false
	}
	for _, c := range f.commands {
		if c[0] == args[0] {
			return args[0], 0, true
		}
	}
	f.usage(stderr)
	names := make([]string, len(f.commands))
	for i, c := range f.commands {
		names[i] = c[0]
	}
	fmt.Fprintf(stderr, "crw skill %s: error: invalid command %q (choose from %s)\n", f.name, args[0], strings.Join(names, ", "))
	return "", usageExit, false
}

// commandLine is one command's flags and positional arguments.
type commandLine struct {
	*flag.FlagSet
	summary    string
	positional string // the positional argument's name, "" when the command takes none
	min, max   int    // how many positional arguments it takes
}

func newCommandLine(family, command, summary string) *commandLine {
	flags := flag.NewFlagSet("crw skill "+family+" "+command, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.Usage = func() {}
	return &commandLine{FlagSet: flags, summary: summary}
}

// takes declares the positional argument: its name, and how many of it the command takes.
func (c *commandLine) takes(name string, min, max int) *commandLine {
	c.positional, c.min, c.max = name, min, max
	return c
}

func (c *commandLine) usage(w io.Writer) {
	line := "usage: " + c.Name() + " [flags]"
	switch {
	case c.positional == "":
	case c.min == 0:
		line += " [" + c.positional + "]"
	default:
		line += " " + c.positional
	}
	fmt.Fprintf(w, "%s\n\n%s\n", line, c.summary)
	hasFlags := false
	c.VisitAll(func(*flag.Flag) { hasFlags = true })
	if hasFlags {
		fmt.Fprintln(w, "\nflags:")
		c.SetOutput(w)
		c.PrintDefaults()
		c.SetOutput(io.Discard)
	}
}

// parse reads args. It returns the positional arguments and -1, or the exit status: 0 after the
// usage was asked for, 2 after a usage error.
func (c *commandLine) parse(args []string, stdout, stderr io.Writer) ([]string, int) {
	err := c.Parse(args)
	positionals := c.Args()
	switch {
	case errors.Is(err, flag.ErrHelp):
		c.usage(stdout)
		return nil, 0
	case err != nil:
	case len(positionals) < c.min:
		err = fmt.Errorf("the argument %s is required", c.positional)
	case len(positionals) > c.max:
		err = fmt.Errorf("unexpected arguments: %s", strings.Join(positionals[c.max:], " "))
	default:
		return positionals, -1
	}
	c.usage(stderr)
	fmt.Fprintf(stderr, "%s: error: %v\n", c.Name(), err)
	return nil, usageExit
}

var hookProbe = family{name: "hook-probe", description: `Read-only probe for the Codex hook contract in references/hook-contract.md.

Replay proves this parser and this decision table. It is not evidence that the host invoked
a hook or honored its output; that evidence comes from a real run and is recorded separately.
Replay also cross-checks the recorded host observations under fixtures/host against the
capability record each one names. That compares two recordings of the same host; it re-runs
nothing and starts no session.`, commands: [][2]string{
	{"observe", "report the host's hook schemas and registrations; opens nothing for writing and starts no session"},
	{"decide", "apply the contract's Stop decision to one observation and print the hook output"},
	{"replay", "check every fixture against its recorded expectation"},
}}

var parentTitle = family{name: "parent-title", description: `Compute a project parent's task title from the product family its project actually carries.

crw-plan/references/integrations.md#set-the-app-presentation-and-record owns the rule: a
project parent's Codex task title leads with the linked project's product-family label in
brackets, spelled exactly as Linear spells it. The label is data, so nothing here maps, expands or
case-folds it, and there is no table of known families to drift.

The caller supplies the family candidates already scoped to the product-family label group. A
label may contain "[" or "]", so a boundary is only ever acted on when it was constructed from the
verified family or named verbatim by the caller. A decision here is a proposal and never evidence
that a task is named anything, and it assumes the caller has already matched this task to this
project by their stable IDs. replay proves this module agrees with its recorded expectations, and
proves nothing about a title having been written, displayed, or read back from a real host.`, commands: [][2]string{
	{"decide", "read one request as JSON on stdin and print the decision; exit 2 when the request itself is unreadable"},
	{"readback", "classify a rename readback: verified, mismatch or unread"},
	{"replay", "run every fixture against its recorded expectation, and fail when a decision has no fixture reaching it"},
}}

var startPolicy = family{name: "start-policy", description: `Check a start-policy record's two closed-vocabulary fields against the contract.`, commands: [][2]string{
	{"vocabulary", "print the declared values and the legal pairings"},
	{"check", "check a record's two closed-vocabulary fields"},
	{"selftest", "check the vocabulary and the recorded negative cases"},
}}
