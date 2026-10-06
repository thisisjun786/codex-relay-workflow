// Package manage implements the crw manage command: the operating tools of a
// management session as a product surface. Each subcommand lives in its own file and
// registers itself from init(), so a later issue adds a file without editing this one.
package manage

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// usageExit is the status of a command line this command cannot use.
const usageExit = 2

// Command is one crw manage subcommand.
type Command struct {
	Name    string
	Summary string
	Run     func(ctx context.Context, e *Env, args []string) int
}

// Env is what a subcommand runs with: the process's streams and the seams a test
// replaces instead of the host's.
type Env struct {
	Stdin      io.Reader
	Stdout     io.Writer
	Stderr     io.Writer
	Getenv     func(string) string
	Now        func() time.Time
	Executable string
}

// coreRegistry is every registered subcommand, in registration order.
var coreRegistry []Command

// Register adds a subcommand. A subcommand's file calls it from init(), so a later
// issue adds a command without editing a shared table; two commands with one name are
// a programming error and panic.
func Register(c Command) {
	for _, existing := range coreRegistry {
		if existing.Name == c.Name {
			panic("manage: the command " + c.Name + " is already registered")
		}
	}
	coreRegistry = append(coreRegistry, c)
}

// coreCommands is the registered commands, sorted by name.
func coreCommands() []Command {
	out := append([]Command(nil), coreRegistry...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// coreNames is the registered command names, sorted.
func coreNames() []string {
	names := make([]string, 0, len(coreRegistry))
	for _, c := range coreCommands() {
		names = append(names, c.Name)
	}
	return names
}

// Run is crw manage. No argument and an unknown name are usage errors (exit 2); -h,
// --help and help write the usage and the registered commands to stdout and exit 0.
func Run(ctx context.Context, args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	e := &Env{Stdin: stdin, Stdout: stdout, Stderr: stderr, Getenv: os.Getenv, Now: time.Now, Executable: coreExecutable()}
	if len(args) == 0 {
		coreUsage(stderr)
		fmt.Fprintln(stderr, "crw manage: error: the following arguments are required: command")
		return usageExit
	}
	switch args[0] {
	case "-h", "--help", "help":
		coreUsage(stdout)
		return 0
	}
	for _, c := range coreCommands() {
		if c.Name == args[0] {
			// A configuration file this product cannot use is refused before the subcommand
			// runs, so no subcommand silently continues with the defaults. config is the one
			// command that names the file itself (--config), so it reports its own refusal
			// once it has read its flag.
			if c.Name != coreConfigCommand.Name {
				if err := coreConfigError(e); err != nil {
					fmt.Fprintf(stderr, "crw manage: error: %v\n", err)
					return usageExit
				}
			}
			return c.Run(ctx, e, args[1:])
		}
	}
	coreUsage(stderr)
	fmt.Fprintf(stderr, "crw manage: error: invalid command %q (choose from '%s')\n", args[0], strings.Join(coreNames(), "', '"))
	return usageExit
}

// coreUsage writes the usage line and one line per registered command, name-sorted.
func coreUsage(w io.Writer) {
	fmt.Fprintln(w, "usage: crw manage [-h] {"+strings.Join(coreNames(), ",")+"} ...")
	for _, c := range coreCommands() {
		fmt.Fprintf(w, "  %s\t%s\n", c.Name, c.Summary)
	}
}

// coreExecutable is the running binary, which the relay helper runs again in its relay
// mode. A failure to name it leaves the field empty and surfaces as a relay error
// rather than at start-up.
func coreExecutable() string {
	path, err := os.Executable()
	if err != nil {
		return ""
	}
	return path
}
