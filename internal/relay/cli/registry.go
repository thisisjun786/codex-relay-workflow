package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Args contains the positional arguments and parsed flags of a registered command.
type Args struct {
	Positionals []string
	Flags       *flag.FlagSet
	// Set names the flags that appeared on the command line, so an empty value is still asked.
	Set map[string]bool
}

// String is the value of a string flag, and whether it was given at all (argparse's None).
func (a Args) String(name string) (string, bool) {
	value := a.Flags.Lookup(name).Value.String()
	return value, a.Set[name]
}

func (a Args) Bool(name string) bool { return a.Flags.Lookup(name).Value.String() == "true" }

type Command struct {
	Name string
	// Required lists flags argparse declares required=True.
	Required []string
	Flags    func(*flag.FlagSet)
	// Exempt commands answer without the store default discovery would pick
	// (_refuse_ambiguous_state's exemption list).
	Exempt bool
	Run    func(context.Context, Services, Args) (any, error)
}

// Commands lists only implemented operations; later domain ports append theirs.
var Commands = []Command{doctorCommand, storeIdentityCommand, storeChallengeCommand, showCommand, statusCommand}

// Registered reports whether this build implements the relay command name.
func Registered(name string) bool { return slices.Contains(allNames(), name) }

// UsageError is SystemExit2: {"error": "usage", "detail": ...} with its own exit code.
type UsageError struct {
	Detail string
	Code   int
}

func (e *UsageError) Error() string { return e.Detail }

// PayloadExit is a completed answer that is still a refusal: the payload is printed whole.
type PayloadExit struct {
	Payload contract.OrderedObject
	Code    int
}

func (e *PayloadExit) Error() string { return fmt.Sprint(get(e.Payload, "detail")) }

// ExitPayload lets another relay package print this answer whole (delivery.PayloadError).
func (e *PayloadExit) ExitPayload() (contract.OrderedObject, int) { return e.Payload, e.Code }

// HostError is an unexpected failure whose Python class name is known, so the host envelope
// can carry Python's f"{type(error).__name__}: {error}" unchanged.
type HostError struct {
	Class  string
	Detail string
}

func (e *HostError) Error() string { return e.Class + ": " + e.Detail }

// Version and Build are set by cmd/crw; doctor reports them in its runtime block.
var Version, Build = "dev", ""

const parserExit = 2 // argparse's exit status for a command line it cannot parse

func Execute(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	return ExecuteAs(ctx, "codex-session-relay", argv, stdout, stderr)
}

// ExecuteAs is cli.main: argparse first (exit 2, usage on stderr), then Services, the
// selection refusal, the handler, and one JSON document on stdout for every other ending.
func ExecuteAs(ctx context.Context, argv0 string, argv []string, stdout, stderr io.Writer) int {
	prog := argv0
	if i := strings.LastIndex(prog, "/"); i >= 0 {
		prog = prog[i+1:]
	}
	globalUsage := "usage: " + prog + " [-h] [--state STATE] [--socket SOCKET]\n" +
		strings.Repeat(" ", len("usage: "+prog)) + " [--kind-module KIND_MODULE] [--json]\n" +
		strings.Repeat(" ", len("usage: "+prog)) + " {" + commandNames() + "} ..."
	parseErrorAs := func(who, usage, message string) int {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "%s: error: %s\n", who, message)
		return parserExit
	}
	parseError := func(usage, message string) int { return parseErrorAs(prog, usage, message) }
	globals := flag.NewFlagSet("relay", flag.ContinueOnError)
	globals.SetOutput(io.Discard)
	state := globals.String("state", "", "state directory")
	socket := globals.String("socket", "", "App Server socket")
	globals.Bool("json", true, "JSON output")
	kindModules := &stringsFlag{}
	globals.Var(kindModules, "kind-module", "register a fault kind (repeatable)")
	if err := globals.Parse(argv); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, globalUsage)
			return contract.ExitOk
		}
		return parseError(globalUsage, argparseMessage(err))
	}
	remaining := globals.Args()
	if len(remaining) == 0 {
		return parseError(globalUsage, "the following arguments are required: command")
	}
	if slices.Contains(delivery.CommandNames(), remaining[0]) {
		code, _ := delivery.ExecuteAs(ctx, prog, argv, stdout, stderr, func(selection store.StateSelection, socket string) error {
			services := Services{Selection: selection, SocketPath: socket, AdapterRequested: socket != "", Program: program(argv0)}
			refusal, err := selectionRefusal(services)
			if err != nil {
				return err
			}
			if refusal != nil {
				return &PayloadExit{Payload: refusal, Code: contract.ExitRefused}
			}
			return nil
		})
		return code
	}
	var command *Command
	for i := range Commands {
		if Commands[i].Name == remaining[0] {
			command = &Commands[i]
		}
	}
	if command == nil {
		return parseError(globalUsage, fmt.Sprintf("argument command: invalid choice: %s (choose from %s)", store.PythonRepr(remaining[0]), choices()))
	}
	flags := flag.NewFlagSet(command.Name, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if command.Flags != nil {
		command.Flags(flags)
	}
	commandUsage := "usage: " + prog + " " + command.Name + commandSynopsis(flags)
	if err := flags.Parse(remaining[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprintln(stdout, commandUsage)
			return contract.ExitOk
		}
		return parseErrorAs(prog+" "+command.Name, commandUsage, argparseMessage(err))
	}
	if extra := flags.Args(); len(extra) > 0 {
		return parseError(globalUsage, "unrecognized arguments: "+strings.Join(extra, " "))
	}
	given := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { given[f.Name] = true })
	var missing []string
	for _, name := range command.Required {
		if !given[name] {
			missing = append(missing, "--"+name)
		}
	}
	if len(missing) > 0 {
		return parseErrorAs(prog+" "+command.Name, commandUsage, "the following arguments are required: "+strings.Join(missing, ", "))
	}
	result, err := run(ctx, command, argv0, *state, *socket, *kindModules, Args{Flags: flags, Set: given})
	return emit(stdout, stderr, result, err)
}

func run(ctx context.Context, command *Command, argv0, state, socket string, kindModules []string, args Args) (any, error) {
	selection, err := store.ResolveStateDir(state, socket)
	if err != nil {
		if errors.Is(err, store.ErrNoHome) {
			return nil, &HostError{Class: "RuntimeError", Detail: "Could not determine home directory."}
		}
		return nil, err
	}
	services := Services{Selection: selection, SocketPath: socket, AdapterRequested: socket != "", Program: program(argv0)}
	if !command.Exempt {
		refusal, err := selectionRefusal(services)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return nil, &PayloadExit{Payload: refusal, Code: contract.ExitRefused}
		}
	}
	// _import_kind_modules runs after the selection refusal and before the handler.
	if err := importKindModules(kindModules); err != nil {
		return nil, err
	}
	return command.Run(ctx, services, args)
}

// importKindModules is _import_kind_modules. A Go build has no Python modules to import, so
// until kinds come from a static registry (todo 22) every named module is one this process
// cannot import, answered with the exception importlib.import_module raises for it.
func importKindModules(names []string) error {
	// The first name decides: Python imports them in order and stops at the first failure.
	if len(names) > 0 {
		name := names[0]
		if name == "" {
			return &HostError{Class: "ValueError", Detail: "Empty module name"}
		}
		if strings.HasPrefix(name, ".") {
			return &HostError{Class: "TypeError", Detail: "the 'package' argument is required to perform a relative import for " + store.PythonRepr(name)}
		}
		top, _, _ := strings.Cut(name, ".")
		return &UsageError{
			Detail: "--kind-module " + store.PythonRepr(name) + " could not be imported: No module named " + store.PythonRepr(top),
			Code:   contract.ExitUsage,
		}
	}
	return nil
}

func emit(stdout, stderr io.Writer, result any, err error) int {
	var usage *UsageError
	var payload *PayloadExit
	var refused *store.RefusedError
	var host *HostError
	code := contract.ExitOk
	switch {
	case err == nil:
	case errors.As(err, &refused):
		result = contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: nullableText(refused.Reason)}, {Key: "detail", Value: refused.Detail}}
		code = contract.ExitRefused
	case errors.As(err, &usage):
		result = contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: usage.Detail}}
		code = usage.Code
	case errors.As(err, &payload):
		result, code = payload.Payload, payload.Code
	case errors.As(err, &host):
		result = contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: host.Error()}}
		code = contract.ExitHost
	default:
		result = contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: err.Error()}}
		code = contract.ExitHost
	}
	if err := contract.Emit(stdout, result); err != nil {
		fmt.Fprintln(stderr, err)
		return contract.ExitHost
	}
	return code
}

// argparseMessage turns a flag package parse failure into argparse's wording.
func argparseMessage(err error) string {
	text := err.Error()
	if name, found := strings.CutPrefix(text, "flag needs an argument: -"); found {
		return "argument --" + strings.TrimPrefix(name, "-") + ": expected one argument"
	}
	if name, found := strings.CutPrefix(text, "flag provided but not defined: -"); found {
		return "unrecognized arguments: --" + strings.TrimPrefix(name, "-")
	}
	// flag: invalid boolean value "1" for -write: parse error
	if rest, found := strings.CutPrefix(text, "invalid boolean value "); found {
		value, name, _ := strings.Cut(rest, " for -")
		name, _, _ = strings.Cut(name, ":")
		return "argument --" + strings.TrimPrefix(name, "-") + ": ignored explicit argument " + store.PythonRepr(strings.Trim(value, `"`))
	}
	return text
}

// allNames is every relay command this build registers: this package's and delivery's.
func allNames() []string {
	names := make([]string, 0, len(Commands))
	for _, command := range Commands {
		names = append(names, command.Name)
	}
	return append(names, delivery.CommandNames()...)
}

func commandNames() string { return strings.Join(allNames(), ",") }

func choices() string {
	names := allNames()
	for i, name := range names {
		names[i] = store.PythonRepr(name)
	}
	return strings.Join(names, ", ")
}

func commandSynopsis(flags *flag.FlagSet) string {
	var parts []string
	flags.VisitAll(func(f *flag.Flag) {
		if boolean, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && boolean.IsBoolFlag() {
			parts = append(parts, "[--"+f.Name+"]")
			return
		}
		parts = append(parts, "[--"+f.Name+" "+strings.ToUpper(strings.ReplaceAll(f.Name, "-", "_"))+"]")
	})
	return " [-h] " + strings.Join(parts, " ")
}

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}
