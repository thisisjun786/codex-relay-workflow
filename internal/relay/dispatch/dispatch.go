// Package dispatch is the relay CLI's one command table and the one path every relay command
// line takes through it: the root parse (once, for the global options), the command's own
// parse, the selected store's checks in cli.main's order (check_start, the selection refusal,
// then admission or --kind-module), the handler, and one JSON document on stdout for every
// ending but a line the parser cannot read (exit 2, usage on stderr).
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
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/selection"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Command is one relay command: its parser, argparse.Specs[Name], reads its line; Run answers it.
type Command struct {
	// Name is the command's parser: a root subcommand, or "service <sub>" for one of service's.
	Name string
	// Run is the handler, reached once the selected store passed the checks below.
	Run func(context.Context, Services, Args) (any, error)
	// Defaults are the values of the options Run reads when the line omits them.
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
	// UsageHelp answers -h/--help with the usage line alone (the capacity and edit-region
	// commands').
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

const parserExit = 2 // the exit status of a command line the parser cannot read

// Execute is the relay CLI for one command line; argv0 is how the CLI was invoked (its base
// name is the program the usage lines name, and Services.Program spells the recovery commands
// with it). The root options come before the command, the command's own after it.
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
	if code, done := parsedLine(stdout, stderr, prog, "", root); done {
		return code
	}
	name, line := root.Remaining[0], root.Remaining[1:]
	var positionals []string
	if name == "service" {
		parent := argparse.Parse(name, line)
		if code, done := parsedLine(stdout, stderr, prog, name, parent); done {
			return code
		}
		positionals = []string{parent.Remaining[0]}
		name += " " + parent.Remaining[0]
		line = parent.Remaining[1:]
	}
	command := table[name]
	if command == nil {
		// A command this build does not register: the parser that read its name refuses it.
		parent, word, who := "", name, prog
		if len(positionals) > 0 {
			parent, word, who = "service", positionals[0], prog+" service"
		}
		fmt.Fprintf(stderr, "%s\n%s: error: argument command: invalid choice: %q (see %s --help)\n", argparse.Usage(prog, parent), who, word, who)
		return parserExit
	}
	parsed := argparse.Parse(name, line)
	if parsed.Help && command.UsageHelp {
		fmt.Fprintln(stdout, argparse.Usage(prog, name))
		return contract.ExitOk
	}
	if code, done := parsedLine(stdout, stderr, prog, name, parsed); done {
		return code
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

// parsedLine ends a command line the parser answered: its help on stdout (exit 0), or why it
// cannot be read on stderr (exit 2). done is false for a line to run.
func parsedLine(stdout, stderr io.Writer, prog, name string, parsed argparse.Result) (code int, done bool) {
	switch {
	case parsed.Help:
		fmt.Fprint(stdout, argparse.Help(prog, name))
		return contract.ExitOk, true
	case parsed.Message != "":
		fmt.Fprint(stderr, parsed.Error(prog, name))
		return parserExit, true
	}
	return 0, false
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
		// A state directory that cannot be resolved (a ~user with no home, a relative --state
		// under a working directory that is gone) is one host answer for every family.
		resolved, err := store.ResolveStateDir(g.state, g.socket)
		if err != nil {
			return nil, Host("the state directory cannot be resolved: " + Detail(err))
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

// kindModules are the modules --kind-module may name, each with what naming it installs.
// codex_session_relay.projects declares the product publication kinds (a credential holder names
// it on routing's holder commands, docs/relay/product-routing.md); json and os.path are accepted
// and install nothing (docs/port/known-defects.md).
var kindModules = map[string]func(){"json": nil, "os.path": nil, "codex_session_relay.projects": nil}

// OnKindModule makes naming module run install (the fault package installs the product
// declarations when codex_session_relay.projects is named).
func OnKindModule(module string, install func()) {
	if _, known := kindModules[module]; !known {
		panic("dispatch: unknown kind module " + module)
	}
	kindModules[module] = install
}

// importKindModules installs the named kind modules in order. The first name that is not one
// stops the command: an empty or relative name as a host error (exit 3), any other as a usage
// error (exit 4).
func importKindModules(names []string) error {
	for _, name := range names {
		install, known := kindModules[name]
		switch {
		case known:
			if install != nil {
				install()
			}
			continue
		case name == "" || strings.HasPrefix(name, "."):
			return Host(fmt.Sprintf("--kind-module %q is not an absolute module name", name))
		}
		declared := make([]string, 0, len(kindModules))
		for module := range kindModules {
			declared = append(declared, module)
		}
		slices.Sort(declared)
		return &UsageError{
			Detail: fmt.Sprintf("--kind-module %q is not a module this relay knows; it knows %s", name, strings.Join(declared, ", ")),
			Code:   contract.ExitUsage,
		}
	}
	return nil
}
