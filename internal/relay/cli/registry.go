package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
)

// Args contains the positional arguments and parsed flags of a registered command.
type Args struct {
	Positionals []string
	Flags       *flag.FlagSet
}

type Command struct {
	Name  string
	Flags func(*flag.FlagSet)
	Run   func(context.Context, Args) (any, error)
}

// Commands starts empty: domain ports register only implemented operations.
var Commands = []Command{}

func Execute(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	usage := func() {
		fmt.Fprintln(stderr, "usage: crw relay [--state DIR] [--socket PATH] [--kind-module MODULE] [--json] <command> [flags]")
	}
	globals := flag.NewFlagSet("relay", flag.ContinueOnError)
	globals.SetOutput(stderr)
	globals.String("state", "", "state directory")
	globals.String("socket", "", "App Server socket")
	globals.Bool("json", true, "JSON output")
	globals.Var(&stringsFlag{}, "kind-module", "register a fault kind (repeatable)")
	globals.Usage = usage
	if err := globals.Parse(argv); err != nil {
		usage()
		return contract.ExitUsage
	}
	remaining := globals.Args()
	if len(remaining) == 0 {
		usage()
		return contract.ExitUsage
	}
	for _, command := range Commands {
		if command.Name != remaining[0] {
			continue
		}
		flags := flag.NewFlagSet(command.Name, flag.ContinueOnError)
		flags.SetOutput(stderr)
		if command.Flags != nil {
			command.Flags(flags)
		}
		if err := flags.Parse(remaining[1:]); err != nil {
			usage()
			return contract.ExitUsage
		}
		result, err := command.Run(ctx, Args{Positionals: flags.Args(), Flags: flags})
		if err != nil {
			fmt.Fprintln(stderr, err)
			return contract.ExitHost
		}
		if err := contract.Emit(stdout, result); err != nil {
			fmt.Fprintln(stderr, err)
			return contract.ExitHost
		}
		return contract.ExitOk
	}
	usage()
	return contract.ExitUsage
}

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
