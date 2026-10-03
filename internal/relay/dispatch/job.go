package dispatch

import (
	"context"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/job"
)

// Constructed when the table is read, without startup registration. Jobs never
// select or admit the relay database.
func jobCommands() []Command {
	var commands []Command
	for _, verb := range []string{"run", "list", "get", "cancel", "off", "on", "status", "drain", "removal"} {
		commands = append(commands, Command{Name: "job " + verb, Run: runJobCommand, Unselected: true})
	}
	return commands
}

// Flags remain subject to the common parser. Missing operands are judged after
// help and bad flags, so they cannot hide a parser refusal.
func jobOperands(name string, line []string) (flags, operands []string, missing string) {
	flags = line
	switch name {
	case "job get", "job cancel":
		if len(line) == 0 || argparse.Optional(line[0]) {
			return flags, nil, "id"
		}
		return line[1:], line[:1], ""
	case "job run":
		sep := slices.Index(line, "--")
		if sep < 0 || sep == len(line)-1 {
			return flags, nil, "-- <command...>"
		}
		return line[:sep], line[sep+1:], ""
	}
	return flags, nil, ""
}

func runJobCommand(_ context.Context, _ Services, args Args) (any, error) {
	cwd, err := syscall.Getwd()
	if err != nil {
		return nil, err
	}
	opts := job.CLIOptions{Verb: args.Positionals[0], JSON: args.Bool("json")}
	if opts.Verb == "get" || opts.Verb == "cancel" {
		opts.ID = args.Positionals[1]
	}
	if opts.Verb == "run" {
		opts.Command = args.Positionals[1:]
	}
	if args.Given("note") {
		v := args.Text("note")
		opts.Note = &v
	}
	if args.Given("tail") {
		v := args.Text("tail")
		opts.Tail = &v
	}
	if args.Given("session") {
		v := args.Text("session")
		opts.Session = &v
	}
	result, err := job.RunParsedCLI(opts, cwd, os.LookupEnv, time.Now)
	if err != nil {
		return nil, err
	}
	answer := contract.OrderedObject{{Key: "out", Value: result.Out}}
	if result.Code != 0 {
		return nil, &PayloadExit{Payload: answer, Code: result.Code}
	}
	return answer, nil
}

func jobCommand(name string) bool { return strings.HasPrefix(name, "job ") }
