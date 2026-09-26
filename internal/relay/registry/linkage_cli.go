package registry

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The linkage CLI commands (cli.py:797-890, :1192-1232, :4132-4391).

// projectReadingLimits is assignment.PROJECT_READING_LIMITS.
const projectReadingLimits = "A reading of what this project's children report, not a completion verdict. The strongest " +
	"state is complete_candidate: integration and verification are the parent's judgment and are " +
	"not visible here. A child's own goal status is never consulted; each assignment's state is " +
	"derived from receipts, verdicts and marks against the current head, generation and " +
	"revision. unreadable, unregistered and ambiguous are three different answers and none of " +
	"them means finished."

// pythonErrorText is f"{type(error).__name__}: {error}" for a failed store read.
func pythonErrorText(err error) string { return store.PythonSQLiteError(err) }

// ProjectState is AssignmentView.project_state.
func (r *Registry) ProjectState(ctx context.Context, project string) contract.OrderedObject {
	reading := func(state string, readable bool, attached, outstanding []string, extra ...contract.Field) contract.OrderedObject {
		out := contract.OrderedObject{{Key: "state", Value: state}, {Key: "readable", Value: readable}, {Key: "projectKey", Value: project},
			{Key: "attached", Value: strList(attached)}, {Key: "outstanding", Value: strList(outstanding)}}
		out = append(out, extra...)
		return append(out, contract.Field{Key: "limits", Value: projectReadingLimits})
	}
	attached, err := r.Attached(ctx, project, sql.NullString{}, sql.NullString{})
	var outstanding []string
	var owners []contract.OrderedObject
	if err == nil {
		outstanding, err = r.Outstanding(ctx, project, sql.NullString{})
	}
	if err == nil {
		owners, err = r.Owners(ctx, scopeProject, project)
	}
	if err != nil {
		return reading("unreadable", false, []string{}, []string{},
			contract.Field{Key: "basis", Value: "the project's assignments could not be read: " + pythonErrorText(err)})
	}
	if len(owners) > 1 {
		competing := make([]string, len(owners))
		for i, o := range owners {
			competing[i] = field(o, "taskId")
		}
		sortStrings(competing)
		return reading("ambiguous", true, attached, outstanding, contract.Field{Key: "competingOwners", Value: strList(competing)},
			contract.Field{Key: "basis", Value: "the project has more than one live owner, so which parent this reading is about is not decided here"})
	}
	if len(attached) == 0 {
		return reading("unregistered", true, []string{}, []string{},
			contract.Field{Key: "basis", Value: "no live assignment is attached to this project, which is not the same as every assignment being finished"})
	}
	view := NewAssignmentView(r)
	unfinished := []any{}
	for _, rid := range outstanding {
		state, err := view.State(ctx, rid)
		if err != nil {
			return reading("unreadable", false, attached, outstanding,
				contract.Field{Key: "basis", Value: "the unfinished set could not be expanded: " + pythonErrorText(err)})
		}
		unfinished = append(unfinished, contract.OrderedObject{{Key: "relationshipId", Value: rid}, {Key: "state", Value: field(state, "state")}})
	}
	if len(unfinished) > 0 {
		return reading("incomplete", true, attached, outstanding, contract.Field{Key: "unfinished", Value: unfinished},
			contract.Field{Key: "basis", Value: fmt.Sprintf("%d of %d live assignments are unfinished", len(unfinished), len(attached))})
	}
	return reading("complete_candidate", true, attached, []string{}, contract.Field{Key: "unfinished", Value: []any{}},
		contract.Field{Key: "basis", Value: fmt.Sprintf("all %d live assignments in this project report a finished state", len(attached))})
}

// withEnforcement is cli._with_enforcement: name the guard indexes this store could not install.
func withEnforcement(r *Registry, answer contract.OrderedObject) contract.OrderedObject {
	if len(r.Store.UnenforcedIndexes) == 0 {
		return answer
	}
	unenforced := []any{}
	for _, index := range r.Store.UnenforcedIndexes {
		unenforced = append(unenforced, contract.OrderedObject{{Key: "index", Value: index.Index}, {Key: "detail", Value: index.Detail}})
	}
	return append(copyObject(answer), contract.Field{Key: "unenforcedIndexes", Value: unenforced})
}

// linkageExit is a PayloadExit: the whole answer printed with its own exit code.
type linkageExit struct {
	payload contract.OrderedObject
	code    int
}

func (e *linkageExit) Error() string { return "linkage refusal" }

// ExitPayload prints the answer whole.
func (e *linkageExit) ExitPayload() (contract.OrderedObject, int) { return e.payload, e.code }

func endpoint(p parsed, prefix string) Endpoint {
	return Endpoint{TaskID: p.text(prefix + "task"), HostID: p.text(prefix + "host"), Cwd: p.optional(prefix + "cwd"), CXCSession: p.optional(prefix + "cxc-session")}
}

var linkageCommands = []command{
	{name: "linkage-bind", options: []option{{name: "role", required: true, choices: roleChoices}, {name: "scope", required: true},
		{name: "task", required: true}, {name: "host", required: true}, {name: "cwd"}, {name: "cxc-session"}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return r.BindScopeAs(ctx, p.text("role"), p.text("scope"), endpoint(p, ""), Active)
		}},
	{name: "linkage-supervise", options: []option{{name: "initiative", required: true}, {name: "project", required: true},
		{name: "supervisor-task", required: true}, {name: "supervisor-host", required: true}, {name: "supervisor-cwd"}, {name: "supervisor-cxc-session"},
		{name: "parent-task", required: true}, {name: "parent-host", required: true}, {name: "parent-cwd"}, {name: "parent-cxc-session"},
		{name: "kind", def: linkExec, choices: []string{linkExec, linkReference}}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return r.RegisterSupervision(ctx, p.text("initiative"), p.text("project"), endpoint(p, "supervisor-"), endpoint(p, "parent-"), p.text("kind"))
		}},
	{name: "linkage-peer", options: []option{{name: "left-project", required: true}, {name: "left-task", required: true}, {name: "left-host", required: true},
		{name: "right-project", required: true}, {name: "right-task", required: true}, {name: "right-host", required: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return r.RegisterPeer(ctx, p.text("left-project"), Endpoint{TaskID: p.text("left-task"), HostID: p.text("left-host")},
				p.text("right-project"), Endpoint{TaskID: p.text("right-task"), HostID: p.text("right-host")})
		}},
	{name: "linkage-attach", options: []option{{name: "relationship", required: true}, {name: "project", required: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return r.AttachIssue(ctx, p.text("relationship"), p.text("project"))
		}},
	{name: "linkage-outstanding", options: []option{{name: "project", required: true}, {name: "task"}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			outstanding, err := r.Outstanding(ctx, p.text("project"), p.optional("task"))
			if err != nil {
				return nil, err
			}
			return contract.OrderedObject{{Key: "projectKey", Value: p.text("project")}, {Key: "taskId", Value: nullable(p.optional("task"))},
				{Key: "outstanding", Value: strList(outstanding)}}, nil
		}},
	{name: "linkage-completion", options: []option{{name: "project", required: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return withEnforcement(r, r.ProjectState(ctx, p.text("project"))), nil
		}},
	{name: "linkage-handover", options: []option{{name: "role", required: true, choices: []string{roleSupervisor, roleParent}},
		{name: "scope", required: true}, {name: "expect-task", required: true}, {name: "task", required: true}, {name: "host", required: true},
		{name: "cwd"}, {name: "cxc-session"}, {name: "acknowledge", multi: true}, {name: "evidence", required: true}, {name: "actor", required: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return r.Handover(ctx, p.text("role"), p.text("scope"), p.text("expect-task"), endpoint(p, ""), p.values["acknowledge"],
				p.text("evidence"), p.text("actor"))
		}},
	{name: "linkage-directive", options: []option{{name: "scope-kind", required: true, choices: []string{scopeInitiative, scopeProject, scopeIssue}},
		{name: "scope", required: true}, {name: "from-task", required: true}, {name: "from-scope", required: true}, {name: "link", required: true},
		{name: "digest", required: true}, {name: "reference"}, {name: "purpose", choices: SupervisorPurposes()}, {name: "correlation"}},
		run: cmdLinkageDirective},
	{name: "linkage-settle", options: []option{{name: "directive", required: true}, {name: "disposition", required: true, choices: []string{"chosen", "superseded"}},
		{name: "actor", required: true}, {name: "reason"}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return r.SettleDirective(ctx, p.text("directive"), p.text("disposition"), p.text("actor"), p.optional("reason"))
		}},
	{name: "linkage-down", options: []option{{name: "scope-kind", required: true, choices: []string{scopeInitiative, scopeProject, scopeIssue}}, {name: "scope", required: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			return withEnforcement(r, r.Down(ctx, p.text("scope-kind"), p.text("scope"))), nil
		}},
	{name: "linkage-up", options: []option{{name: "task"}, {name: "issue"}, {name: "relationship"}, {name: "scope"}},
		exclusive: []string{"task", "issue", "relationship"}, run: cmdLinkageUp},
	{name: "linkage-counterpart", options: []option{{name: "from-task", required: true}, {name: "to-task", required: true}, {name: "from-scope"},
		{name: "quoted-scope"}, {name: "quoted-revision", integer: true}},
		run: func(ctx context.Context, r *Registry, p parsed) (any, error) {
			q := CounterpartQuery{QuotedScope: p.optional("quoted-scope"), FromScope: p.optional("from-scope")}
			if p.set["quoted-revision"] {
				q.QuotedRevision = sql.NullInt64{Int64: p.integer("quoted-revision"), Valid: true}
			}
			return r.Counterpart(ctx, p.text("from-task"), p.text("to-task"), q), nil
		}},
}

// cmdLinkageDirective is cli.cmd_linkage_directive.
func cmdLinkageDirective(ctx context.Context, r *Registry, p parsed) (any, error) {
	reference := p.optional("reference")
	correlation, purpose := p.optional("correlation"), p.optional("purpose")
	if correlation.String != "" && purpose.String == "" {
		return nil, &usage{detail: "--correlation is part of an envelope pointer, so it requires --purpose", code: contract.ExitUsage}
	}
	if purpose.String != "" {
		if reference.String != "" {
			return nil, &usage{detail: "--purpose derives the envelope pointer, so it cannot be given with --reference", code: contract.ExitUsage}
		}
		derived, err := DirectiveReference(purpose.String, p.text("link"), p.text("digest"), correlation.String, correlation.Valid)
		if err != nil {
			return nil, err
		}
		reference = sql.NullString{String: derived, Valid: true}
	}
	return r.RecordDirective(ctx, p.text("scope-kind"), p.text("scope"), p.text("from-task"), p.text("from-scope"), p.text("link"), p.text("digest"), reference)
}

// cmdLinkageUp is cli.cmd_linkage_up.
func cmdLinkageUp(ctx context.Context, r *Registry, p parsed) (any, error) {
	if p.text("scope") != "" && p.text("task") == "" {
		return nil, &linkageExit{payload: contract.OrderedObject{{Key: "ok", Value: false}, {Key: "reason", Value: "bad_invocation"},
			{Key: "detail", Value: "--scope chooses between the scopes one TASK owns, so it goes with" +
				" --task. With --issue or --relationship the starting scope is already" +
				" decided and --scope would be silently ignored."}}, code: contract.ExitRefused}
	}
	return withEnforcement(r, r.Up(ctx, UpSelector{Task: p.optional("task"), Issue: p.optional("issue"),
		Relationship: p.optional("relationship"), Scope: p.optional("scope")})), nil
}

func init() { commands = append(commands, linkageCommands...) }
