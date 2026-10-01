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
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
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

// TruthyString is Python's `if args.name` for an argparse string: both an omitted
// option (None) and an explicitly empty value ("") are false.
func (a Args) TruthyString(name string) (string, bool) {
	value, _ := a.String(name)
	return value, value != ""
}

func (a Args) Bool(name string) bool { return a.Flags.Lookup(name).Value.String() == "true" }

// Command is one relay command of this package's table. Its arguments are parsed by its
// argparse spec (argparse.Specs[Name]); Flags declares where their values are stored.
type Command struct {
	Name  string
	Flags func(*flag.FlagSet)
	// Exempt commands answer without the store default discovery would pick
	// (_refuse_ambiguous_state's exemption list).
	Exempt bool
	Run    func(context.Context, Services, Args) (any, error)
}

// Commands lists only implemented operations; later domain ports append theirs.
var Commands = []Command{daemonCommand, serviceCommand, guardEvaluateCommand, doctorCommand, storeIdentityCommand, storeChallengeCommand, showCommand, statusCommand, reportingShowCommand, reportingDeriveCommand, supervisorStandingCommand, supervisorSelectCommand, supervisorReportRecordedCommand, supervisorStageCommand, supervisorShowCommand, supervisorSendCommand, supervisorReadCommand, mergeEvidenceCommand}

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

// ExitPayload lets another relay package print this answer whole (registry.PayloadError, delivery.PayloadError).
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
func ExecuteAs(ctx context.Context, argv0 string, argv []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if value := recover(); value != nil {
			if failure, ok := value.(*evidence.PythonError); ok {
				code = emit(stdout, stderr, nil, failure)
			} else {
				panic(value)
			}
		}
	}()
	prog := argv0
	if i := strings.LastIndex(prog, "/"); i >= 0 {
		prog = prog[i+1:]
	}
	globalUsage := argparse.Usage(prog, "")
	parseErrorAs := func(who, usage, message string) int {
		fmt.Fprintln(stderr, usage)
		fmt.Fprintf(stderr, "%s: error: %s\n", who, message)
		return parserExit
	}
	parseError := func(usage, message string) int { return parseErrorAs(prog, usage, message) }
	root := argparse.Parse("", argv)
	if root.Help {
		fmt.Fprint(stdout, argparse.Help(prog, ""))
		return contract.ExitOk
	}
	if root.Message != "" {
		fmt.Fprint(stderr, root.Error(prog, ""))
		return parserExit
	}
	remaining := root.Remaining
	if len(root.Unknown) > 0 {
		childName := remaining[0]
		child := argparse.Parse(childName, remaining[1:])
		if childName == "service" && child.Message == "" && !child.Help {
			childName += " " + child.Remaining[0]
			child = argparse.Parse(childName, append(append([]string{}, child.Unknown...), child.Remaining[1:]...))
		}
		if child.Help {
			fmt.Fprint(stdout, argparse.Help(prog, childName))
			return contract.ExitOk
		}
		if child.Message != "" && !child.Global {
			fmt.Fprint(stderr, child.Error(prog, childName))
			return parserExit
		}
		message := "unrecognized arguments: " + strings.Join(root.Unknown, " ")
		if child.Message != "" {
			message += " " + strings.TrimPrefix(child.Message, "unrecognized arguments: ")
		}
		return parseError(globalUsage, message)
	}
	state, socket := "", ""
	if values := root.Values["state"]; len(values) > 0 {
		state = values[0]
	}
	if values := root.Values["socket"]; len(values) > 0 {
		socket = values[0]
	}
	kindModules := root.Values["kind-module"]
	argv = root.RootArgs()
	ctx = readOnlyContext(ctx, remaining)
	// cli.main's _ownership_preflight: a writable command opens its store once the selection
	// refusal passed, then imports --kind-module, all before its handler reads its own
	// arguments; the handler's store is that store.
	drains := admitsBeforeHandler(remaining[0]) && !store.ReadOnlyCommand(ctx)
	// cli.py main's check_start, for the families' dispatch: after the command's own argument
	// parse and before the selection refusal, --kind-module and the handler. The intent
	// commands' dispatch runs its own (delivery's fenced markers), and a command that answers
	// without the selected store is checked with none.
	startChecked := !store.ReadOnlyCommand(ctx) && !strings.HasPrefix(remaining[0], "intent-")
	checkStart := func(selection store.StateSelection, socket string) error {
		if !startChecked || selection.Path == "" {
			return nil
		}
		return store.CheckStartLikeFence(ctx, selection.DBPath())
	}
	ctx, admitted := store.WithAdmitted(ctx)
	defer func() { _ = admitted.Release() }()
	admit := func(selection store.StateSelection, socket string) error {
		st, err := store.Open(ctx, selection.DBPath(), socket)
		if err != nil {
			return err
		}
		if err = kindModuleRefusal(kindModules); err == nil {
			admitted.Hold(st, selection.DBPath(), socket)
			return nil
		}
		if e := st.Close(); e != nil {
			err = errors.Join(err, e)
		}
		return err
	}
	refusal := func(selection store.StateSelection, socket string) error {
		if err := checkStart(selection, socket); err != nil {
			return err
		}
		services := Services{Selection: selection, SocketPath: socket, AdapterRequested: socket != "", Program: program(argv0)}
		refusal, err := selectionRefusal(services)
		if err != nil {
			return err
		}
		if refusal != nil {
			return &PayloadExit{Payload: refusal, Code: contract.ExitRefused}
		}
		return nil
	}
	if slices.Contains(faults.Names(), remaining[0]) {
		code, _ := faults.ExecuteAs(ctx, prog, argv, stdout, stderr, func(selection store.StateSelection, socket string) error {
			if err := refusal(selection, socket); err != nil || !drains {
				return err
			}
			return admit(selection, socket)
		})
		return code
	}
	if slices.Contains(registry.Names(), remaining[0]) {
		return registry.ExecuteAs(ctx, prog, argv, stdout, stderr, func(selection store.StateSelection, socket string) error {
			if err := refusal(selection, socket); err != nil {
				return err
			}
			if drains {
				return admit(selection, socket)
			}
			return kindModuleRefusal(kindModules)
		})
	}
	if slices.Contains(delivery.CommandNames(), remaining[0]) {
		code, _ := delivery.ExecuteAs(ctx, prog, argv, stdout, stderr, func(selection store.StateSelection, socket string) error {
			if remaining[0] != "ack-proof" {
				if err := refusal(selection, socket); err != nil {
					return err
				}
			}
			if drains && selection.Path != "" {
				return admit(selection, socket)
			}
			return kindModuleRefusal(kindModules)
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
		return parseError(globalUsage, fmt.Sprintf("argument command: invalid choice: %s (choose from %s)", pyvalue.StrRepr(remaining[0]), choices()))
	}
	commandArgs := remaining[1:]
	parserName := command.Name
	var positionals []string
	if command.Name == "service" {
		parent := argparse.Parse("service", commandArgs)
		if parent.Help {
			fmt.Fprint(stdout, argparse.Help(prog, "service"))
			return contract.ExitOk
		}
		if parent.Message != "" {
			fmt.Fprint(stderr, parent.Error(prog, "service"))
			return parserExit
		}
		positionals = []string{parent.Remaining[0]}
		parserName += " " + parent.Remaining[0]
		commandArgs = append(append([]string{}, parent.Unknown...), parent.Remaining[1:]...)
	}
	flags := flag.NewFlagSet(parserName, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	if command.Flags != nil {
		command.Flags(flags)
	}
	given, code, done := parseRelayArgs(prog, flags, commandArgs, stdout, stderr)
	if done {
		return code
	}
	result, err := run(ctx, command, argv0, state, socket, kindModules, admitIf(drains, admit), Args{Flags: flags, Set: given, Positionals: positionals})
	return emit(stdout, stderr, result, err)
}

// admitIf is admit for a command that drains, nil for any other.
func admitIf(drains bool, admit func(store.StateSelection, string) error) func(store.StateSelection, string) error {
	if drains {
		return admit
	}
	return nil
}

// run is cli.main for a command of this package's table: check_start, the selection refusal,
// then admit (the writable open and --kind-module) for a command admitted before its handler,
// or --kind-module alone, then the handler.
func run(ctx context.Context, command *Command, argv0, state, socket string, kindModules []string, admit func(store.StateSelection, string) error, args Args) (any, error) {
	services := Services{SocketPath: socket, AdapterRequested: socket != "", Program: program(argv0)}
	if command.Name != "merge-evidence" {
		selection, err := store.ResolveStateDir(state, socket)
		if err != nil {
			if errors.Is(err, store.ErrNoHome) {
				return nil, &HostError{Class: "RuntimeError", Detail: "Could not determine home directory."}
			}
			return nil, err
		}
		services.Selection = selection
	}
	// cli.py main runs the lock-free check_start (ownership.check_start, with --socket) for
	// every command that is neither read-only nor answers without the selected store, before
	// the selection refusal, --kind-module and the handler's own refusals: another runtime's
	// store, or one mid-transition, is refused first.
	if !store.ReadOnlyCommand(ctx) && command.Name != "merge-evidence" {
		if err := store.CheckStartLikeFence(ctx, services.Selection.DBPath()); err != nil {
			return nil, err
		}
	}
	if !command.Exempt {
		refusal, err := selectionRefusal(services)
		if err != nil {
			return nil, err
		}
		if refusal != nil {
			return nil, &PayloadExit{Payload: refusal, Code: contract.ExitRefused}
		}
	}
	if admit != nil && !command.Exempt {
		if err := admit(services.Selection, socket); err != nil {
			return nil, err
		}
		return command.Run(ctx, services, args)
	}
	// _import_kind_modules runs after the selection refusal and before the handler.
	if err := importKindModules(kindModules); err != nil {
		return nil, err
	}
	return command.Run(ctx, services, args)
}

// importKindModules checks the static module registry before entering a command.
func importKindModules(names []string) error {
	// The first name decides: Python imports them in order and stops at the first failure.
	if len(names) > 0 {
		for _, candidate := range names {
			if candidate == "codex_session_relay.projects" {
				faults.InstallProductDeclarations()
			}
			if candidate == "" {
				return &HostError{Class: "ValueError", Detail: "Empty module name"}
			}
			if strings.HasPrefix(candidate, ".") {
				return &HostError{Class: "TypeError", Detail: "the 'package' argument is required to perform a relative import for " + pyvalue.StrRepr(candidate)}
			}
			if faults.RegisteredModule(candidate) {
				continue
			}
			missing := candidate
			parts := strings.Split(candidate, ".")
			for i := 1; i < len(parts); i++ {
				prefix := strings.Join(parts[:i], ".")
				if prefix == "codex_session_relay" || faults.RegisteredModule(prefix) {
					continue
				}
				missing = prefix
				break
			}
			return &UsageError{
				Detail: "--kind-module " + pyvalue.StrRepr(candidate) + " could not be imported: No module named " + pyvalue.StrRepr(missing),
				Code:   contract.ExitUsage,
			}
		}
		return nil
	}
	return nil
}

// Delegated commands run their selection check before importing modules. Give their
// runners the same complete CLI error envelope as the ordinary command path.
func kindModuleRefusal(names []string) error {
	err := importKindModules(names)
	if err == nil {
		return nil
	}
	var usage *UsageError
	if errors.As(err, &usage) {
		return &PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: usage.Detail}}, Code: usage.Code}
	}
	return &PayloadExit{Payload: contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: err.Error()}}, Code: contract.ExitHost}
}

func emit(stdout, stderr io.Writer, result any, err error) int {
	var usage *UsageError
	var payload *PayloadExit
	var refused *store.RefusedError
	var host *HostError
	code := contract.ExitOk
	switch {
	case err == nil:
	case store.EncodeError(err) != nil:
		// A str sqlite3 or an identity hash cannot encode, raised through whatever wrapped it.
		result = contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: store.EncodeError(err).HostDetail()}}
		code = contract.ExitHost
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

// allNames is every relay command this build registers: this package's, registry's, delivery's and faults'.
func allNames() []string {
	names := make([]string, 0, len(Commands))
	for _, command := range Commands {
		names = append(names, command.Name)
	}
	names = append(names, registry.Names()...)
	return append(append(names, delivery.CommandNames()...), faults.Names()...)
}

func choices() string {
	names := allNames()
	for i, name := range names {
		names[i] = pyvalue.StrRepr(name)
	}
	return strings.Join(names, ", ")
}

type stringsFlag []string

func (s *stringsFlag) String() string { return fmt.Sprint([]string(*s)) }
func (s *stringsFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

// admitsBeforeHandler reports whether a writable command admits its store at dispatch, before
// its handler reads its own arguments (cli.py main: _ownership_preflight opens services.store,
// then the handler). The service and daemon commands open theirs in their own recovery;
// managed-start and the intent commands open their own admitted connection.
func admitsBeforeHandler(command string) bool {
	switch command {
	case "service", "daemon", "managed-start":
		return false
	}
	return !strings.HasPrefix(command, "intent-")
}
