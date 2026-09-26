//go:build dev

// Package ci holds the repository's CI checks, run as `crw-dev ci <check>`. It is built only
// with -tags dev and never ships in a release archive.
package ci

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// Check runs one CI check with its arguments and returns the exit status.
type Check func(args []string, stdout, stderr io.Writer) int

// Checks is the `crw-dev ci` command table.
var Checks = map[string]Check{
	"contracts":  Contracts,
	"gate":       Gate,
	"operations": OperationsContract,
	"plugin":     Plugin,
	"scope":      Scope,
	"validate":   Validate,
}

// Run dispatches `crw-dev ci <check> [args]`.
func Run(args []string, stdout, stderr io.Writer) int {
	names := make([]string, 0, len(Checks))
	for name := range Checks {
		names = append(names, name)
	}
	sort.Strings(names)
	usage := "usage: crw-dev ci {" + strings.Join(names, ",") + "} ..."
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintln(stderr, "crw-dev ci: error: the following arguments are required: check")
		return 2
	}
	if args[0] == "-h" || args[0] == "--help" {
		fmt.Fprintln(stdout, usage)
		return 0
	}
	check, ok := Checks[args[0]]
	if !ok {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "crw-dev ci: error: invalid choice: %s (choose from %s)\n", pyRepr(args[0]), strings.Join(names, ", "))
		return 2
	}
	return check(args[1:], stdout, stderr)
}
