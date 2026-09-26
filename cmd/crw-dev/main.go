//go:build dev

// Command crw-dev carries the repository's development tooling (CI checks and the like). It
// is built only with -tags dev and is never part of a release archive.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/dev/ci"
)

// commands is the top-level command tree; each entry owns its own arguments.
var commands = map[string]func(args []string, stdout, stderr io.Writer) int{
	"ci": ci.Run,
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	const usage = "usage: crw-dev {ci} ..."
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev: error: the following arguments are required: command")
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	command, ok := commands[args[0]]
	if !ok {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "crw-dev: error: invalid command %q\n", args[0])
		return 2
	}
	return command(args[1:], stdout, stderr)
}
