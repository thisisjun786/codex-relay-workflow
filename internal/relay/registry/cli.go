package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math/big"
	"os"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/pyerr"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The relay CLI commands this package owns (cli.py:4045-4125, :4393): register,
// settings-record, settings-show, generation-open, generation-bind, admit-turn,
// relationship-status and relationship-resume. Parsing follows argparse: a command line it
// cannot parse exits 2 with usage on stderr; every other ending prints one JSON document.

type parsed struct {
	values  map[string][]string
	set     map[string]bool
	numbers map[string]any
	// writes are register's settings, parsed once by its precheck, before the handler.
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

func (p parsed) integer(name string) *big.Int {
	return p.numbers[name].(*big.Int)
}

// command is one relay command of this package: its registration (its name names its argparse
// spec, argparse.Specs[Name]; its defaults are the argparse defaults of the options it reads
// unset) and how its handler reaches the store.
type command struct {
	dispatch.Command
	run func(context.Context, *Registry, parsed) (any, error)
	// precheck is the part of a handler that refuses its own arguments before it touches the
	// store. cli.main decides when that is (handle): a write form refuses them only after
	// _ownership_preflight opened the store, a read-only form before its lazy Services.store.
	precheck func(*parsed) error
	// read answers without constructing a Store (dispositions-show).
	read func(context.Context, store.StateSelection, parsed) (any, error)
}

var commands = []command{
	{Command: dispatch.Command{Name: "register"}, run: cmdRegister, precheck: registerPrecheck},
	{Command: dispatch.Command{Name: "settings-record", Defaults: map[string]any{"source": "creation_result"}}, run: cmdSettingsRecord, precheck: settingsRecordPrecheck},
	{Command: dispatch.Command{Name: "settings-show", ReadOnly: true}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		return r.SettingsShow(ctx, p.text("task"))
	}},
	{Command: dispatch.Command{Name: "generation-open", Defaults: map[string]any{"reason": "needs_changes_revision"}}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		g, err := r.OpenGeneration(ctx, p.text("relationship"), p.text("dispatch-request-id"), p.text("reason"), p.optional("dispatch-turn-id"))
		return g.Record(), err
	}},
	{Command: dispatch.Command{Name: "generation-bind", Defaults: map[string]any{"source": "dispatch_receipt"}}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		g, err := r.BindAnchor(ctx, p.text("relationship"), p.integer("generation"), p.text("dispatch-turn-id"), p.text("source"))
		return g.Record(), err
	}},
	{Command: dispatch.Command{Name: "admit-turn"}, run: cmdAdmitTurn},
	{Command: dispatch.Command{Name: "relationship-status"}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		x, err := r.SetStatus(ctx, p.text("relationship"), p.text("status"), p.text("actor"))
		return x.ContractRecord(), err
	}},
	{Command: dispatch.Command{Name: "relationship-resume"}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		x, err := r.Resume(ctx, p.text("relationship"), p.integer("expect-generation"), p.values["expect-artifact-root"], p.values["expect-allowed-recipient"], p.text("actor"))
		return x.ContractRecord(), err
	}},
	{Command: dispatch.Command{Name: "assignment-show", ReadOnly: true}, run: cmdAssignmentShow},
	{Command: dispatch.Command{Name: "assignment-find", ReadOnly: true}, run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
		return assignmentView(r).ForIssue(ctx, p.text("issue"))
	}},
	{Command: dispatch.Command{Name: "dispositions-show", ReadOnly: true}, read: cmdDispositionsShow},
	{Command: dispatch.Command{Name: "assignment-mark"},
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
	report, err := ReadDispositions(ctx, selection, project, relationship)
	if err != nil {
		return nil, err
	}
	if readable, _ := getField(report, "readable"); readable != true {
		return nil, &DispositionsExit{report}
	}
	return report, nil
}

// family is this package's commands' family: a failure no other ending classifies reads as
// itself, an integer sqlite3 could not bind as its OverflowError however it was wrapped.
var family = &dispatch.Family{HostDetail: func(err error) string {
	var overflow *argparse.IntegerOverflow
	if errors.As(err, &overflow) {
		return overflow.Error()
	}
	return err.Error()
}}

// register adds commands to the relay command table, each answered by handle.
func register(commands ...command) {
	for _, c := range commands {
		registration := c.Command
		registration.Run = func(ctx context.Context, services dispatch.Services, args dispatch.Args) (any, error) {
			return handle(ctx, services, c, parsedOf(args))
		}
		dispatch.Register(family, registration)
	}
}

func init() { register(append(commands, linkageCommands...)...) }

// parsedOf is the handlers' view of the line: every value given, a declared default for each
// option the line left out.
func parsedOf(args dispatch.Args) parsed {
	p := parsed{values: args.Parsed.Values, set: args.Parsed.Given, numbers: args.Parsed.Numbers}
	for name, value := range args.Defaults {
		if !p.set[name] {
			p.values[name] = []string{value.(string)}
		}
	}
	return p
}

// handle is cli.main for one of this package's commands, once the relay CLI checked the
// selected store (and admitted it, for a write form).
func handle(ctx context.Context, services dispatch.Services, c command, p parsed) (any, error) {
	selection := services.Selection
	// A write form's store is opened before its handler reads its arguments, as the fence's
	// cli.main does in _ownership_preflight (services.store): the store is admitted - initialized
	// when absent (decision 30), refused when another runtime owns it - before any refusal of the
	// form's own arguments, so a refused write form leaves the store it would have written and
	// answers another owner's store with the ownership refusal. A read-only form (and a read,
	// which constructs no Store) keeps Services.store lazy: its own argument refusal comes first
	// and an absent store is refused store_absent only by a form that reaches the store
	// (decision 31).
	var s *store.Store
	var err error
	if !store.ReadOnlyCommand(ctx) && c.read == nil {
		if s, err = store.Open(ctx, selection.DBPath(), services.SocketPath); err != nil {
			return nil, err
		}
		defer s.Close()
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
	if s == nil {
		if s, err = store.Open(ctx, selection.DBPath(), services.SocketPath); err != nil {
			return nil, err
		}
		defer s.Close()
	}
	r := &Registry{Store: s, Policy: policy}
	return c.run(ctx, r, p)
}

// settingsJSON is cli._settings_json: a JSON object, or @path to a file holding one.
func settingsJSON(raw string) (contract.OrderedObject, error) {
	data := []byte(raw)
	if path, ok := strings.CutPrefix(raw, "@"); ok {
		content, err := os.ReadFile(path)
		if err != nil {
			if class, message, ok := pyerr.OSError(err); ok {
				return nil, &dispatch.HostError{Class: class, Detail: message}
			}
			return nil, err
		}
		data = content
	}
	if !store.ValidUTF8(data) {
		return nil, &dispatch.HostError{Class: "UnicodeDecodeError", Detail: "'utf-8' codec can't decode the settings"}
	}
	decoded, err := pyjson.Loads(string(data), pyjson.LoadOptions{Python: true})
	if err != nil {
		return nil, &dispatch.HostError{Class: "JSONDecodeError", Detail: err.Error()}
	}
	object, ok := decoded.(contract.OrderedObject)
	if !ok {
		// dict(values) of a non-object: TypeError/ValueError in Python, a host error either way.
		return nil, &dispatch.HostError{Class: "TypeError", Detail: "the settings are " + pyvalue.TypeName(decoded) + ", not a JSON object"}
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
			return refuse(contract.RefusalRoleBindingMismatch, "%s holds live bindings at %s; one task holds one role, so there is no single role to register settings against", pyvalue.StrRepr(w.task), pyvalue.Repr(contested))
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
		return &dispatch.UsageError{Detail: "--clear-exception drops the citation and --exception records one; state one", Code: contract.ExitUsage}
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
	return contract.OrderedObject{{Key: "relationship", Value: rid}, {Key: "generation", Value: json.Number(generation.String())},
		{Key: "turn", Value: turn}, {Key: "evidence", Value: "explicit_admission"}}, nil
}

// AdmitExplicitly is admission.admit_explicitly.
func (r *Registry) AdmitExplicitly(ctx context.Context, rid string, generation any, turn, actor, detail string) error {
	if strings.TrimSpace(turn) == "" {
		return &dispatch.HostError{Class: "ValueError", Detail: "an admitted turn needs an exact turn id"}
	}
	if strings.TrimSpace(actor) == "" {
		return &dispatch.HostError{Class: "ValueError", Detail: "an explicit admission records who made it"}
	}
	now := r.now()
	return r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Querier(ctx)
		number, err := argparse.SQLiteInteger(argparse.IntegerValue(generation))
		if err != nil {
			return err
		}
		generation = number
		var anchor sql.NullString
		err = q.QueryRowContext(ctx, "SELECT dispatch_turn_id FROM generations WHERE relationship_id=? AND execution_generation=?", rid, generation).Scan(&anchor)
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
