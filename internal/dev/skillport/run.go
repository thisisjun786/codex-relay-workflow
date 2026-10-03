//go:build dev

package skillport

import (
	"flag"
	"fmt"
	"io"
	"strings"
)

// Run is the skillport command: stage and check.
func Run(args []string, stdout, stderr io.Writer) int {
	return run(args, stdout, stderr, DefaultOrigin())
}

func run(args []string, stdout, stderr io.Writer, origin Origin) int {
	usage := func() int {
		fmt.Fprintln(stderr, "usage: skillport stage --source DIR FOLDER... | check [--source DIR]  (both take --root DIR)")
		return 2
	}
	if len(args) == 0 || (args[0] != "stage" && args[0] != "check") {
		return usage()
	}
	flags := flag.NewFlagSet("skillport "+args[0], flag.ContinueOnError)
	flags.SetOutput(stderr)
	root := flags.String("root", ".", "repository root")
	source := flags.String("source", "", "the extracted CXC v0.2.40 tree")
	if flags.Parse(args[1:]) != nil || (args[0] == "check") != (flags.NArg() == 0) || (args[0] == "stage" && *source == "") {
		return usage()
	}
	src := Source{Dir: *source, Origin: origin}
	if args[0] == "stage" {
		names, err := Stage(*root, src, flags.Args())
		for _, name := range names {
			fmt.Fprintln(stdout, "staged", name)
		}
		if err != nil {
			fmt.Fprintln(stderr, "skillport:", err)
			return 1
		}
		return 0
	}
	against := &src
	if *source == "" {
		against = nil
	}
	n, problems := Check(*root, against)
	if len(problems) > 0 {
		fmt.Fprintln(stderr, strings.Join(problems, "\n"))
		return 1
	}
	fmt.Fprintf(stdout, "Checked %d staged skills against their records (fidelity only; crw-dev ci validate also validates skills and links).\n", n)
	return 0
}
