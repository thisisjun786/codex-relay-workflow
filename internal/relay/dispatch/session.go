package dispatch

import (
	"context"
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/session"
)

// Like jobCommands, these rows are constructed when the table is read.
func sessionCommands() []Command {
	var commands []Command
	for _, verb := range []string{"current", "bind", "source"} {
		commands = append(commands, Command{Name: "session " + verb, Run: runSessionCommand, Unselected: true})
	}
	return commands
}

func sessionCommand(name string) bool { return strings.HasPrefix(name, "session ") }

func sessionOperands(name string, line []string) (flags, operands []string, missing string) {
	if name != "session source" {
		return line, nil, ""
	}
	if len(line) == 0 || argparse.Optional(line[0]) {
		return line, nil, "absolute-worktree"
	}
	return line[1:], line[:1], ""
}

func runSessionCommand(_ context.Context, services Services, args Args) (any, error) {
	cwd, err := syscall.Getwd()
	if err != nil {
		return nil, err
	}
	opts := session.Options{Command: args.Positionals[0], JSON: args.Bool("json")}
	if opts.Command == "source" {
		opts.SourceRoot = args.Positionals[1]
	}
	result := session.Run(opts, cwd, os.LookupEnv)
	if result.Note != "" {
		diagnostics := services.Stderr
		if diagnostics == nil {
			diagnostics = os.Stderr
		}
		fmt.Fprintln(diagnostics, "crw: "+result.Note)
	}
	answer := contract.OrderedObject{{Key: "out", Value: result.Out}}
	if result.Code != 0 {
		return nil, &PayloadExit{Payload: answer, Code: result.Code}
	}
	return answer, nil
}
