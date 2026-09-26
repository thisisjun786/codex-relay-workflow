package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/pyerr"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay CLI commands this package owns (cli.py:4045-4125, :4393): register,
// settings-record, settings-show, generation-open, generation-bind, admit-turn,
// relationship-status and relationship-resume. Parsing follows argparse: a command line it
// cannot parse exits 2 with usage on stderr; every other ending prints one JSON document.

type option struct {
	name     string
	required bool
	multi    bool // action="append"
	flag     bool // action="store_true"
	integer  bool // type=int
	choices  []string
	def      string
}

type parsed struct {
	values map[string][]string
	set    map[string]bool
	// writes are register's settings, parsed once before the store is opened.
	writes []settingsWrite
}

func (p parsed) text(name string) string {
	if v := p.values[name]; len(v) > 0 {
		return v[len(v)-1]
	}
	return ""
}

// optional is an argparse value that may be None.
func (p parsed) optional(name string) sql.NullString {
	if v := p.values[name]; len(v) > 0 {
		return sql.NullString{String: v[len(v)-1], Valid: true}
	}
	return sql.NullString{}
}

func (p parsed) integer(name string) int64 {
	n, _ := strconv.ParseInt(strings.TrimSpace(p.text(name)), 10, 64)
	return n
}

type command struct {
	name    string
	options []option
	run     func(context.Context, *Registry, parsed) (any, error)
	// precheck runs before the store is opened: a handler that refuses its arguments before
	// touching services.store leaves no state directory behind, as Python's lazy Services does.
	precheck func(*parsed) error
	// exclusive names a required mutually exclusive group (argparse add_mutually_exclusive_group).
	exclusive []string
	// read answers without constructing a Store (dispositions-show).
	read func(context.Context, store.StateSelection, parsed) (any, error)
}

var roleChoices = []string{"supervisor", "parent", "child"}

var commands = []command{
	{"register", []option{
		{name: "parent-task", required: true}, {name: "parent-host", required: true}, {name: "parent-cwd"}, {name: "parent-cxc-session"},
		{name: "child-task", required: true}, {name: "child-host", required: true}, {name: "child-cwd"}, {name: "child-cxc-session"},
		{name: "issue", required: true}, {name: "artifact-root", required: true, multi: true},
		{name: "allowed-recipient", required: true, multi: true}, {name: "scope-ref"},
		{name: "dispatch-request-id", required: true}, {name: "dispatch-turn-id"}, {name: "supersedes"}, {name: "project"},
		{name: "parent-settings"}, {name: "child-settings"},
		{name: "parent-role", choices: roleChoices}, {name: "child-role", choices: roleChoices},
		{name: "parent-exception"}, {name: "child-exception"},
	}, cmdRegister, registerPrecheck, nil, nil},
	{"settings-record", []option{{name: "task", required: true}, {name: "settings", required: true},
		{name: "source", def: "creation_result"}, {name: "role", choices: roleChoices}, {name: "exception"},
		{name: "clear-exception", flag: true}}, cmdSettingsRecord, settingsRecordPrecheck, nil, nil},
	{"settings-show", []option{{name: "task", required: true}}, func(ctx context.Context, r *Registry, p parsed) (any, error) {
		return r.SettingsShow(ctx, p.text("task"))
	}, nil, nil, nil},
	{"generation-open", []option{{name: "relationship", required: true}, {name: "dispatch-request-id", required: true},
		{name: "reason", def: "needs_changes_revision"}, {name: "dispatch-turn-id"}}, func(ctx context.Context, r *Registry, p parsed) (any, error) {
		g, err := r.OpenGeneration(ctx, p.text("relationship"), p.text("dispatch-request-id"), p.text("reason"), p.optional("dispatch-turn-id"))
		return g.Record(), err
	}, nil, nil, nil},
	{"generation-bind", []option{{name: "relationship", required: true}, {name: "generation", required: true, integer: true},
		{name: "dispatch-turn-id", required: true}, {name: "source", def: "dispatch_receipt"}}, func(ctx context.Context, r *Registry, p parsed) (any, error) {
		g, err := r.BindAnchor(ctx, p.text("relationship"), p.integer("generation"), p.text("dispatch-turn-id"), p.text("source"))
		return g.Record(), err
	}, nil, nil, nil},
	{"admit-turn", []option{{name: "relationship", required: true}, {name: "generation", required: true, integer: true},
		{name: "turn", required: true}, {name: "actor", required: true}, {name: "reason", def: ""}}, cmdAdmitTurn, nil, nil, nil},
	{"relationship-status", []option{{name: "relationship", required: true}, {name: "status", required: true, choices: []string{"paused", "cancelled", "archived"}},
		{name: "actor", required: true}}, func(ctx context.Context, r *Registry, p parsed) (any, error) {
		x, err := r.SetStatus(ctx, p.text("relationship"), p.text("status"), p.text("actor"))
		return x.ContractRecord(), err
	}, nil, nil, nil},
	{"relationship-resume", []option{{name: "relationship", required: true}, {name: "expect-generation", required: true, integer: true},
		{name: "expect-artifact-root", required: true, multi: true}, {name: "expect-allowed-recipient", required: true, multi: true},
		{name: "actor", required: true}}, func(ctx context.Context, r *Registry, p parsed) (any, error) {
		x, err := r.Resume(ctx, p.text("relationship"), p.integer("expect-generation"), p.values["expect-artifact-root"], p.values["expect-allowed-recipient"], p.text("actor"))
		return x.ContractRecord(), err
	}, nil, nil, nil},
	{name: "assignment-show", options: []option{{name: "relationship"}, {name: "issue"}}, run: cmdAssignmentShow},
	{name: "assignment-find", options: []option{{name: "issue", required: true}}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		return assignmentView(r).ForIssue(ctx, p.text("issue"))
	}},
	{name: "dispositions-show", options: []option{{name: "project"}, {name: "relationship"}}, exclusive: []string{"project", "relationship"}, read: cmdDispositionsShow},
	{name: "assignment-mark", options: []option{{name: "relationship", required: true}, {name: "mark", required: true, choices: []string{"merged"}},
		{name: "evidence", required: true}, {name: "actor", required: true}, {name: "expected-event", required: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return assignmentView(r).Mark(ctx, p.text("relationship"), p.text("mark"), p.text("evidence"), p.text("actor"), p.text("expected-event"))
		}},
}

// assignmentView is Services.assignments: the default send budget and the system clock.
func assignmentView(r *Registry) *AssignmentView { return NewAssignmentView(r) }

// cmdAssignmentShow is cli.cmd_assignment_show: --issue answers for_issue, otherwise state.
func cmdAssignmentShow(ctx context.Context, r *Registry, p parsed) (any, error) {
	if p.set["issue"] {
		return assignmentView(r).ForIssue(ctx, p.text("issue"))
	}
	if !p.set["relationship"] {
		return nil, refuse(contract.RefusalUnregisteredRelationship, "no relationship None")
	}
	return assignmentView(r).State(ctx, p.text("relationship"))
}

// cmdDispositionsShow is cli.cmd_dispositions_show: no Store is constructed, and an unreadable
// store refuses (exit 2) with the whole answer.
func cmdDispositionsShow(ctx context.Context, selection store.StateSelection, p parsed) (any, error) {
	var project, relationship *string
	if p.set["project"] {
		v := p.text("project")
		project = &v
	} else {
		v := p.text("relationship")
		relationship = &v
	}
	report := ReadDispositions(ctx, selection, project, relationship)
	if readable, _ := getField(report, "readable"); readable != true {
		return nil, &DispositionsExit{report}
	}
	return report, nil
}

type globalFlags struct{ state, socket string }

func globals(argv []string) (globalFlags, []string, error) {
	var g globalFlags
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		name, value, inline := strings.Cut(arg, "=")
		switch name {
		case "--state", "--socket", "--kind-module":
			if !inline {
				if i+1 >= len(argv) {
					return g, nil, fmt.Errorf("argument %s: expected one argument", name)
				}
				i++
				value = argv[i]
			}
			if name == "--state" {
				g.state = value
			} else if name == "--socket" {
				g.socket = value
			}
		case "--json":
		default:
			return g, argv[i:], nil
		}
	}
	return g, nil, nil
}

// usageError is argparse's exit: status 2, usage and message on stderr, nothing on stdout.
type usageError struct{ usage, message string }

func (e *usageError) Error() string { return e.message }

func (c command) usage(prog string) string {
	parts := []string{"usage: " + prog + " " + c.name + " [-h]"}
	if c.exclusive != nil {
		var group []string
		for _, name := range c.exclusive {
			group = append(group, "--"+name+" "+strings.ToUpper(strings.ReplaceAll(name, "-", "_")))
		}
		return strings.Join(append(parts, "("+strings.Join(group, " | ")+")"), " ")
	}
	for _, o := range c.options {
		metavar := strings.ToUpper(strings.ReplaceAll(o.name, "-", "_"))
		if o.choices != nil {
			metavar = "{" + strings.Join(o.choices, ",") + "}"
		}
		piece := "--" + o.name
		if !o.flag {
			piece += " " + metavar
		}
		if !o.required {
			piece = "[" + piece + "]"
		}
		parts = append(parts, piece)
	}
	return strings.Join(parts, " ")
}

func (c command) parse(prog string, argv []string) (parsed, error) {
	p := parsed{values: map[string][]string{}, set: map[string]bool{}}
	find := func(name string) *option {
		for i := range c.options {
			if c.options[i].name == name {
				return &c.options[i]
			}
		}
		return nil
	}
	var unrecognized []string
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		if arg == "-h" || arg == "--help" {
			return p, &usageError{usage: c.usage(prog), message: "help"}
		}
		name, value, inline := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		o := (*option)(nil)
		if strings.HasPrefix(arg, "--") {
			o = find(name)
		}
		if o == nil {
			unrecognized = append(unrecognized, arg)
			continue
		}
		if o.flag {
			if contains(c.exclusive, o.name) {
				for _, other := range c.exclusive {
					if other != o.name && p.set[other] {
						return p, &usageError{c.usage(prog), "argument --" + o.name + ": not allowed with argument --" + other}
					}
				}
			}
			p.values[o.name] = []string{"true"}
			p.set[o.name] = true
			continue
		}
		if !inline {
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "--") {
				return p, &usageError{c.usage(prog), "argument --" + o.name + ": expected one argument"}
			}
			i++
			value = argv[i]
		}
		if o.choices != nil && !contains(o.choices, value) {
			quoted := make([]string, len(o.choices))
			for i, c := range o.choices {
				quoted[i] = pyStr(c)
			}
			return p, &usageError{c.usage(prog), "argument --" + o.name + ": invalid choice: " + pyStr(value) + " (choose from " + strings.Join(quoted, ", ") + ")"}
		}
		if o.integer {
			if _, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err != nil {
				return p, &usageError{c.usage(prog), "argument --" + o.name + ": invalid int value: " + pyStr(value)}
			}
		}
		if contains(c.exclusive, o.name) {
			for _, other := range c.exclusive {
				if other != o.name && p.set[other] {
					return p, &usageError{c.usage(prog), "argument --" + o.name + ": not allowed with argument --" + other}
				}
			}
		}
		if o.multi {
			p.values[o.name] = append(p.values[o.name], value)
		} else {
			p.values[o.name] = []string{value}
		}
		p.set[o.name] = true
	}
	var missing []string
	for _, o := range c.options {
		if o.required && !p.set[o.name] {
			missing = append(missing, "--"+o.name)
		}
		if !p.set[o.name] && o.def != "" {
			p.values[o.name] = []string{o.def}
		}
	}
	if len(missing) > 0 {
		return p, &usageError{c.usage(prog), "the following arguments are required: " + strings.Join(missing, ", ")}
	}
	if c.exclusive != nil {
		given := false
		for _, name := range c.exclusive {
			given = given || p.set[name]
		}
		if !given {
			flags := make([]string, len(c.exclusive))
			for i, name := range c.exclusive {
				flags[i] = "--" + name
			}
			return p, &usageError{c.usage(prog), "one of the arguments " + strings.Join(flags, " ") + " is required"}
		}
	}
	if len(unrecognized) > 0 {
		return p, &usageError{"usage: " + prog + " [-h] [--state STATE] [--socket SOCKET] ...", "unrecognized arguments: " + strings.Join(unrecognized, " ")}
	}
	return p, nil
}

// usage is SystemExit2 printed as {"error": "usage", "detail"} with its own exit code.
type usage struct {
	detail string
	code   int
}

func (e *usage) Error() string { return e.detail }

// Names lists this package's relay commands, in cli.py's add_parser order.
func Names() []string {
	names := make([]string, len(commands))
	for i, c := range commands {
		names[i] = c.name
	}
	return names
}

// SelectionCheck is cli's _refuse_ambiguous_state for the resolved selection: nil, or an error
// the caller's emit understands (a payload refusal). It runs before the handler, as in cli.main.
type SelectionCheck func(selection store.StateSelection, socket string) error

// Execute runs one of this package's commands as the codex-session-relay console script.
func Execute(ctx context.Context, argv []string, stdout, stderr io.Writer) int {
	return ExecuteAs(ctx, "codex-session-relay", argv, stdout, stderr, nil)
}

// ExecuteAs runs one of this package's relay commands, as cli.main does for it: prog is the
// program name argparse prints, check the selection refusal the relay CLI applies first.
func ExecuteAs(ctx context.Context, prog string, argv []string, stdout, stderr io.Writer, check SelectionCheck) int {
	g, rest, err := globals(argv)
	if err != nil || len(rest) == 0 {
		fmt.Fprintln(stderr, "usage: "+prog+" [-h] [--state STATE] [--socket SOCKET] ...")
		fmt.Fprintln(stderr, prog+": error: the following arguments are required: command")
		return 2
	}
	var chosen *command
	for i := range commands {
		if commands[i].name == rest[0] {
			chosen = &commands[i]
		}
	}
	if chosen == nil {
		fmt.Fprintln(stderr, prog+": error: argument command: invalid choice: "+pyStr(rest[0]))
		return 2
	}
	p, err := chosen.parse(prog, rest[1:])
	var bad *usageError
	if errors.As(err, &bad) {
		if bad.message == "help" {
			fmt.Fprintln(stdout, bad.usage)
			return 0
		}
		fmt.Fprintln(stderr, bad.usage)
		fmt.Fprintln(stderr, prog+" "+chosen.name+": error: "+bad.message)
		return 2
	}
	result, err := run(ctx, g, *chosen, p, check)
	return emit(stdout, stderr, result, err)
}

func run(ctx context.Context, g globalFlags, c command, p parsed, check SelectionCheck) (any, error) {
	selection, err := store.ResolveStateDir(g.state, g.socket)
	if err != nil {
		return nil, err
	}
	if check != nil {
		if err := check(selection, g.socket); err != nil {
			return nil, err
		}
	}
	// Taken before any handler, as cli.main does: the snapshot is this PROCESS's, not this
	// question's, so a later edit to the file is not adopted without a restart.
	policy := EnvironmentRolePolicy()
	if c.precheck != nil {
		if err := c.precheck(&p); err != nil {
			return nil, err
		}
	}
	if c.read != nil {
		return c.read(ctx, selection, p)
	}
	s, err := store.Open(ctx, selection.DBPath(), g.socket)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	r := &Registry{Store: s, Policy: policy}
	return c.run(ctx, r, p)
}

// PayloadError is an answer printed whole with its own exit code (cli.PayloadExit).
type PayloadError interface {
	error
	ExitPayload() (contract.OrderedObject, int)
}

func emit(stdout, stderr io.Writer, result any, err error) int {
	code := contract.ExitOk
	var refused *store.RefusedError
	var bad *usage
	var host *HostError
	var payload PayloadError
	switch {
	case err == nil:
	case errors.As(err, &payload):
		result, code = payload.ExitPayload()
	case errors.As(err, &refused):
		var reason any
		if refused.Reason != "" {
			reason = refused.Reason
		}
		result, code = contract.OrderedObject{{Key: "error", Value: "refused"}, {Key: "reason", Value: reason}, {Key: "detail", Value: refused.Detail}}, contract.ExitRefused
	case errors.As(err, &bad):
		result, code = contract.OrderedObject{{Key: "error", Value: "usage"}, {Key: "detail", Value: bad.detail}}, bad.code
	case errors.As(err, &host):
		result, code = contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: host.Error()}}, contract.ExitHost
	default:
		result, code = contract.OrderedObject{{Key: "error", Value: "host"}, {Key: "detail", Value: err.Error()}}, contract.ExitHost
	}
	if err := contract.Emit(stdout, result); err != nil {
		fmt.Fprintln(stderr, err)
		return contract.ExitHost
	}
	return code
}

// settingsJSON is cli._settings_json: a JSON object, or @path to a file holding one.
func settingsJSON(raw string) (contract.OrderedObject, error) {
	data := []byte(raw)
	if path, ok := strings.CutPrefix(raw, "@"); ok {
		content, err := os.ReadFile(path)
		if err != nil {
			if class, message, ok := pyerr.OSError(err); ok {
				return nil, &HostError{Class: class, Detail: message}
			}
			return nil, err
		}
		data = content
	}
	if !store.ValidUTF8(data) {
		return nil, &HostError{Class: "UnicodeDecodeError", Detail: "'utf-8' codec can't decode the settings"}
	}
	if message := store.PythonJSONError(string(data)); message != "" {
		return nil, &HostError{Class: "JSONDecodeError", Detail: message}
	}
	decoded, err := decodeJSON(data)
	if err != nil {
		return nil, &HostError{Class: "JSONDecodeError", Detail: err.Error()}
	}
	object, ok := decoded.(contract.OrderedObject)
	if !ok {
		// dict(values) of a non-object: TypeError/ValueError in Python, a host error either way.
		return nil, &HostError{Class: "TypeError", Detail: "the settings are " + pyTypeName(decoded) + ", not a JSON object"}
	}
	return object, nil
}

type settingsWrite struct {
	task      string
	values    contract.OrderedObject
	role      string
	exception sql.NullString
	establish string
}

// registerPrecheck is cmd_register's first step: settings are parsed once, before the lock and
// before any store exists, so an unreadable @path is answered without creating state.
func registerPrecheck(p *parsed) error {
	for _, side := range []struct{ task, settings, role, exception, establish string }{
		{p.text("parent-task"), p.text("parent-settings"), p.text("parent-role"), "parent-exception", ""},
		{p.text("child-task"), p.text("child-settings"), p.text("child-role"), "child-exception", map[bool]string{true: roleChild}[p.text("project") != ""]},
	} {
		if side.settings == "" {
			continue
		}
		values, err := settingsJSON(side.settings)
		if err != nil {
			return err
		}
		p.writes = append(p.writes, settingsWrite{side.task, values, side.role, p.optional(side.exception), side.establish})
	}
	return nil
}

func cmdRegister(ctx context.Context, r *Registry, p parsed) (any, error) {
	writes := p.writes
	in := Registration{
		Parent:            Endpoint{p.text("parent-task"), p.text("parent-host"), p.optional("parent-cwd"), p.optional("parent-cxc-session")},
		Child:             Endpoint{p.text("child-task"), p.text("child-host"), p.optional("child-cwd"), p.optional("child-cxc-session")},
		IssueKey:          p.text("issue"),
		ArtifactRoots:     p.values["artifact-root"],
		AllowedRecipients: p.values["allowed-recipient"],
		DispatchRequestID: p.text("dispatch-request-id"),
		ScopeRef:          p.optional("scope-ref"),
		DispatchTurnID:    p.optional("dispatch-turn-id"),
		Supersedes:        p.text("supersedes"),
		ProjectKey:        p.text("project"),
	}
	return r.RegisterWithSettings(ctx, in, writes)
}

// RegisterWithSettings is cmd_register: the relationship and both settings records composed
// into one transaction, validated before anything is written.
func (r *Registry) RegisterWithSettings(ctx context.Context, in Registration, writes []settingsWrite) (contract.OrderedObject, error) {
	var record Relationship
	recorded := contract.OrderedObject{}
	err := r.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		if err := r.refuseRoleDisagreement(ctx, writes); err != nil {
			return err
		}
		var err error
		if record, err = r.Register(ctx, in); err != nil {
			return err
		}
		for _, w := range writes {
			citation := Citation{ID: w.exception.String, Set: w.exception.Valid}
			if _, err := r.RecordSettings(ctx, w.task, w.values, "creation_result", w.role, citation); err != nil {
				return err
			}
			recorded = setField(recorded, w.task, "recorded")
		}
		return nil
	})
	if err != nil {
		return nil, r.RecordRefusal(ctx, err)
	}
	payload := record.ContractRecord()
	var settings any
	if len(recorded) > 0 {
		settings = recorded
	}
	return append(payload, contract.Field{Key: "authorizedSettings", Value: settings}), nil
}

// refuseRoleDisagreement is cli._refuse_role_disagreement.
func (r *Registry) refuseRoleDisagreement(ctx context.Context, writes []settingsWrite) error {
	for _, w := range writes {
		settings := copyObject(w.values)
		if err := (TaskSettings{settings}).RequireUsable(); err != nil {
			return err
		}
		if w.role == "" {
			continue
		}
		settings = setField(settings, "citedRole", w.role)
		if w.exception.Valid {
			settings = setField(settings, "citedException", w.exception.String)
		}
		bound, contested, err := boundRole(ctx, r.Store, w.task)
		if err != nil {
			return err
		}
		if contested != nil {
			return refuse(contract.RefusalRoleBindingMismatch, "%s holds live bindings at %s; one task holds one role, so there is no single role to register settings against", pyStr(w.task), rolesList(contested))
		}
		target := bound
		if target == "" {
			target = w.establish
		}
		if target == "" {
			target = w.role
		}
		if finding := CheckBinding(w.role, target, settings, r.Policy); finding != nil {
			return findingRefusal(finding)
		}
	}
	return nil
}

func settingsRecordPrecheck(p *parsed) error {
	if p.set["clear-exception"] && p.optional("exception").Valid {
		return &usage{"--clear-exception drops the citation and --exception records one; state one", contract.ExitUsage}
	}
	return nil
}

func cmdSettingsRecord(ctx context.Context, r *Registry, p parsed) (any, error) {
	exception := p.optional("exception")
	values, err := settingsJSON(p.text("settings"))
	if err != nil {
		return nil, err
	}
	return r.RecordSettings(ctx, p.text("task"), values, p.text("source"), p.text("role"),
		Citation{ID: exception.String, Set: exception.Valid, Clear: p.set["clear-exception"]})
}

// BoundExplicitPrefix is admission.BOUND_EXPLICIT_PREFIX.
const BoundExplicitPrefix = "explicit_admission_bound:"

func cmdAdmitTurn(ctx context.Context, r *Registry, p parsed) (any, error) {
	rid, generation, turn, actor := p.text("relationship"), p.integer("generation"), p.text("turn"), p.text("actor")
	if err := r.AdmitExplicitly(ctx, rid, generation, turn, actor, p.text("reason")); err != nil {
		return nil, err
	}
	return contract.OrderedObject{{Key: "relationship", Value: rid}, {Key: "generation", Value: generation},
		{Key: "turn", Value: turn}, {Key: "evidence", Value: "explicit_admission"}}, nil
}

// AdmitExplicitly is admission.admit_explicitly.
func (r *Registry) AdmitExplicitly(ctx context.Context, rid string, generation int64, turn, actor, detail string) error {
	if strings.TrimSpace(turn) == "" {
		return &HostError{Class: "ValueError", Detail: "an admitted turn needs an exact turn id"}
	}
	if strings.TrimSpace(actor) == "" {
		return &HostError{Class: "ValueError", Detail: "an explicit admission records who made it"}
	}
	now := r.now()
	return r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		var anchor sql.NullString
		err := q.QueryRowContext(ctx, "SELECT dispatch_turn_id FROM generations WHERE relationship_id=? AND execution_generation=?", rid, generation).Scan(&anchor)
		if errors.Is(err, sql.ErrNoRows) {
			return refuse(contract.RefusalUnknownGeneration, "admission needs an existing generation")
		}
		if err != nil {
			return err
		}
		if strings.TrimSpace(anchor.String) == "" {
			return refuse(contract.RefusalUnboundGeneration, "admission needs a bound generation")
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO generation_turns (relationship_id, execution_generation, turn_id,"+
			" evidence, actor, detail, admitted_at) VALUES (?,?,?,?,?,?,?)"+
			" ON CONFLICT(relationship_id, execution_generation, turn_id) DO UPDATE SET"+
			" evidence=excluded.evidence, actor=excluded.actor, detail=excluded.detail,"+
			" admitted_at=excluded.admitted_at WHERE generation_turns.evidence <> excluded.evidence",
			rid, generation, turn, BoundExplicitPrefix+anchor.String, actor, detail, now); err != nil {
			return err
		}
		return journal(ctx, r.Store, "turn_admitted", turn, contract.OrderedObject{{Key: "relationship", Value: rid},
			{Key: "generation", Value: generation}, {Key: "actor", Value: actor}}, now)
	})
}
