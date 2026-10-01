// Package dispatch is the relay CLI's one command table and the one path every relay command
// line takes through it, as cli.py main does: argparse's root parse (once, for the global
// options), the command's own parse, the selected store's checks in cli.main's order
// (check_start, the selection refusal, then admission or --kind-module), the handler, and one
// JSON document on stdout for every ending but a line argparse rejects (exit 2, usage on
// stderr).
//
// Every relay command registers here from its own package (Register): cli's, the registry's
// (with the linkage and merge-turn commands), delivery's (with the marker commands), the faults',
// capacity's, routing's, sync's and the managed commands. Each command carries the attributes
// the pipeline reads (read-only, how it selects its store); a family adds only how its
// unclassified failures read.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/selection"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Command is one relay command: the parser argparse.Specs[Name] reads its line, Run answers it.
type Command struct {
	// Name is the command's parser: a root subcommand, or "service <sub>" for one of service's.
	Name string
	// Run is the handler, reached once the selected store passed the checks below.
	Run func(context.Context, Services, Args) (any, error)
	// Defaults are the argparse defaults of the options Run reads when the line omits them.
	Defaults map[string]any

	// ReadOnly is cli.py's READ_ONLY_COMMANDS, ReadOnlyWhen its _read_only_command for a form
	// its options decide: the store is never created or admitted, and every store.Open under the
	// command reads as Services.store does (store.WithReadOnlyCommand).
	ReadOnly     bool
	ReadOnlyWhen func(Args) bool
	// Unselected commands resolve no state directory at all (merge-evidence, ack-proof).
	Unselected bool
	// SelectsNoStore is _reads_no_selected_store for a form that answers without the store
	// discovery picks (the marker commands'): no check_start, selection refusal or admission.
	SelectsNoStore func(Args) bool
	// Exempt commands are _refuse_ambiguous_state's exemptions: no selection refusal.
	Exempt bool
	// OwnAdmission commands open their own admitted connection (service, daemon, managed-start
	// and the marker commands), so dispatch admits no store before their handler.
	OwnAdmission bool
	// UsageHelp answers -h/--help with the usage line alone, never wrapped: the capacity and
	// edit-region commands' help, which their goldens hold.
	UsageHelp bool

	family *Family
}

// Family is what the commands one package registers share.
type Family struct {
	// HostDetail is the host envelope's detail for a failure no other ending classifies; nil is
	// the error's text.
	HostDetail func(error) string
}

var (
	table = map[string]*Command{}
	order []*Command
)

// Register adds commands to the table. A name registered twice is a programming error.
func Register(family *Family, commands ...Command) {
	for _, command := range commands {
		if _, taken := table[command.Name]; taken {
			panic("dispatch: relay command registered twice: " + command.Name)
		}
		command.family = family
		registered := command
		table[command.Name] = &registered
		order = append(order, &registered)
	}
}

// Registered reports whether this build implements the relay command: a root subcommand (one
// of whose subcommands, for service), or "service <sub>".
func Registered(name string) bool {
	for _, command := range order {
		if command.Name == name || strings.HasPrefix(command.Name, name+" ") {
			return true
		}
	}
	return false
}

// Lookup is the command registered as name (a root subcommand, or "service <sub>").
func Lookup(name string) (Command, bool) {
	command, ok := table[name]
	if !ok {
		return Command{}, false
	}
	return *command, true
}

// Names are the registered commands' names, in registration order.
func Names() []string {
	names := make([]string, len(order))
	for i, command := range order {
		names[i] = command.Name
	}
	return names
}

const parserExit = 2 // argparse's exit status for a command line it cannot parse

// Execute is cli.main for one relay command line; argv0 is how the CLI was invoked (its base
// name is argparse's prog, and Services.Program spells the recovery commands with it).
func Execute(ctx context.Context, argv0 string, argv []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if value := recover(); value != nil {
			failure, ok := value.(*evidence.PythonError)
			if !ok {
				panic(value)
			}
			code = emit(stdout, stderr, nil, failure, nil)
		}
	}()
	prog := argv0
	if i := strings.LastIndex(prog, "/"); i >= 0 {
		prog = prog[i+1:]
	}
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
		// argparse reports unknown root options after the subcommand parsed, so its help and
		// its own errors come first.
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
		return parseError(stderr, prog, message)
	}
	name, line := remaining[0], remaining[1:]
	var positionals []string
	if name == "service" {
		parent := argparse.Parse(name, line)
		if parent.Help {
			fmt.Fprint(stdout, argparse.Help(prog, name))
			return contract.ExitOk
		}
		if parent.Message != "" {
			fmt.Fprint(stderr, parent.Error(prog, name))
			return parserExit
		}
		positionals = []string{parent.Remaining[0]}
		name += " " + parent.Remaining[0]
		line = append(append([]string{}, parent.Unknown...), parent.Remaining[1:]...)
	}
	command := table[name]
	if command == nil {
		return parseError(stderr, prog, fmt.Sprintf("argument command: invalid choice: %s (choose from %s)", pyvalue.StrRepr(remaining[0]), choices()))
	}
	parsed := argparse.Parse(name, line)
	if parsed.Help && command.UsageHelp {
		fmt.Fprintln(stdout, strings.Join(append([]string{"usage:", prog, name}, argparse.Specs[name].Parts...), " "))
		return contract.ExitOk
	}
	if parsed.Help {
		fmt.Fprint(stdout, argparse.Help(prog, name))
		return contract.ExitOk
	}
	if parsed.Message != "" {
		fmt.Fprint(stderr, parsed.Error(prog, name))
		return parserExit
	}
	ctx, admitted := store.WithAdmitted(ctx)
	defer func() { _ = admitted.Release() }()
	global := globals{argv0: argv0, modules: root.Values["kind-module"], admitted: admitted}
	if values := root.Values["state"]; len(values) > 0 {
		global.state = values[0]
	}
	if values := root.Values["socket"]; len(values) > 0 {
		global.socket = values[0]
	}
	result, err := command.run(ctx, global, Args{Parsed: parsed, Positionals: positionals, Defaults: command.Defaults})
	return emit(stdout, stderr, result, err, command.family)
}

func parseError(stderr io.Writer, prog, message string) int {
	fmt.Fprintln(stderr, argparse.Usage(prog, ""))
	fmt.Fprintf(stderr, "%s: error: %s\n", prog, message)
	return parserExit
}

// choices names the registered commands, for a root choice this build does not implement.
func choices() string {
	var names []string
	seen := map[string]bool{}
	for _, command := range order {
		name, _, _ := strings.Cut(command.Name, " ")
		if !seen[name] {
			seen[name] = true
			names = append(names, pyvalue.StrRepr(name))
		}
	}
	return strings.Join(names, ", ")
}

// globals are the root parser's options.
type globals struct {
	argv0, state, socket string
	modules              []string
	admitted             *store.Admitted
}

// run is cli.main between the parse and the handler, in its order: the state directory, then
// check_start for a write form, the selection refusal, and either the writable store's
// admission (opened, then --kind-module imported) or --kind-module alone.
func (c *Command) run(ctx context.Context, g globals, args Args) (any, error) {
	readOnly := c.ReadOnly || c.ReadOnlyWhen != nil && c.ReadOnlyWhen(args)
	if readOnly {
		ctx = store.WithReadOnlyCommand(ctx)
	}
	services := Services{SocketPath: g.socket, AdapterRequested: g.socket != "", Program: selection.Program(g.argv0)}
	selected := !c.Unselected && (c.SelectsNoStore == nil || !c.SelectsNoStore(args))
	if !c.Unselected {
		resolved, err := store.ResolveStateDir(g.state, g.socket)
		if err != nil {
			if errors.Is(err, store.ErrNoHome) {
				return nil, &HostError{Class: "RuntimeError", Detail: "Could not determine home directory."}
			}
			return nil, err
		}
		services.Selection = resolved
	}
	// check_start (lock-free, with --socket) for a command that is neither read-only nor
	// answers without the selected store: another runtime's store, or one mid-transition, is
	// refused before anything else is asked.
	if selected && !readOnly {
		if err := store.CheckStartLikeFence(ctx, services.Selection.DBPath()); err != nil {
			return nil, err
		}
	}
	if selected && !c.Exempt {
		if err := CheckSelection(services); err != nil {
			return nil, err
		}
	}
	if selected && !readOnly && !c.Exempt && !c.OwnAdmission {
		if err := admit(ctx, g, services.Selection); err != nil {
			return nil, err
		}
	} else if err := importKindModules(g.modules); err != nil {
		return nil, err
	}
	return c.Run(ctx, services, args)
}

// admit is _ownership_preflight: a writable command's store is opened (initialized when absent,
// refused when another runtime owns it) and --kind-module imported before its handler reads its
// own arguments; the handler's store.Open is that store.
func admit(ctx context.Context, g globals, selection store.StateSelection) error {
	st, err := store.Open(ctx, selection.DBPath(), g.socket)
	if err != nil {
		return err
	}
	if err = importKindModules(g.modules); err == nil {
		g.admitted.Hold(st, selection.DBPath(), g.socket)
		return nil
	}
	if e := st.Close(); e != nil {
		err = errors.Join(err, e)
	}
	return err
}

// CheckSelection is _refuse_ambiguous_state for the resolved selection: the selection refusal
// as a whole answer (exit 2), or nil.
func CheckSelection(services Services) error {
	refusal, err := selection.Refusal(selection.Services{Selection: services.Selection, SocketPath: services.SocketPath, Program: services.Program})
	if err != nil {
		return err
	}
	if refusal != nil {
		return &PayloadExit{Payload: refusal, Code: contract.ExitRefused}
	}
	return nil
}

// kindModules are the modules --kind-module can import: the relay's static stand-in for
// importlib.import_module, each with what importing it installs. json and os.path are importable
// standard-library probes; codex_session_relay.projects is the publication-kind declaration the
// product-routing tests import.
var kindModules = map[string]func(){"json": nil, "os.path": nil, "codex_session_relay.projects": nil}

// OnKindModule makes importing module run install (the fault package installs the product
// declarations when codex_session_relay.projects is imported).
func OnKindModule(module string, install func()) {
	if _, known := kindModules[module]; !known {
		panic("dispatch: unknown kind module " + module)
	}
	kindModules[module] = install
}

// importKindModules is _import_kind_modules: the modules are imported in order and the first
// that cannot be stops the command.
func importKindModules(names []string) error {
	for _, candidate := range names {
		if install := kindModules[candidate]; install != nil {
			install()
		}
		if candidate == "" {
			return &HostError{Class: "ValueError", Detail: "Empty module name"}
		}
		if strings.HasPrefix(candidate, ".") {
			return &HostError{Class: "TypeError", Detail: "the 'package' argument is required to perform a relative import for " + pyvalue.StrRepr(candidate)}
		}
		if _, known := kindModules[candidate]; known {
			continue
		}
		missing := candidate
		parts := strings.Split(candidate, ".")
		for i := 1; i < len(parts); i++ {
			prefix := strings.Join(parts[:i], ".")
			if _, known := kindModules[prefix]; prefix == "codex_session_relay" || known {
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
