package registry

import (
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// refusalOf is the reason and detail err carries, which is all a caller of the CLI sees of it.
func refusalOf(t *testing.T, err error) (string, string) {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("want a refusal, got %v", err)
	}
	return refused.Reason, refused.Detail
}

func bindProject(t *testing.T, r *Registry, project, task string) {
	t.Helper()
	if _, err := r.BindScopeAs(ctx(), roleParent, project, Endpoint{task, host, ns("/parent"), ns("cxc-" + task)}, Active); err != nil {
		t.Fatal(err)
	}
}

func countRows(t *testing.T, r *Registry, query string) int {
	t.Helper()
	rows, err := r.Store.All(ctx(), query)
	if err != nil {
		t.Fatal(err)
	}
	return len(rows)
}

// PrecheckScope decides, before a child exists, what Register decides after it. Each case pins
// the literal reason and detail Register has always returned, the conflict row the refusal is
// recorded in and the journal entry, so a change to either side shows here.
func TestPrecheckScope_RefusalsAreRegistersRefusals(t *testing.T) {
	for _, c := range []struct {
		name, project, reason, detail string
		setup                         func(*testing.T, *Registry)
		conflictKind, conflictKey     string
		incumbent, challenger         string
	}{
		{
			name: "no parent is registered for the project", project: "P1",
			reason: "unregistered_scope", detail: "project 'P1' has no registered parent, so an issue cannot be attached to it yet",
			conflictKind: "project", conflictKey: "P1", challenger: parent,
		},
		{
			name: "another task is the project's parent", project: "P1",
			setup:  func(t *testing.T, r *Registry) { bindProject(t, r, "P1", "other-parent") },
			reason: "foreign_scope", detail: "issue '" + issue + "' is assigned under parent '" + parent + "', but project 'P1' is executed by 'other-parent'; an issue belongs to its own project",
			conflictKind: "project", conflictKey: "P1", incumbent: "other-parent", challenger: parent,
		},
		{
			name: "the project has two live parents", project: "P1",
			setup: func(t *testing.T, r *Registry) {
				bindProject(t, r, "P1", "owner-a")
				// The store's own unique index forbids a second live owner; drop it to hold the
				// corrupt state the refusal exists for.
				if _, err := r.Store.DB.ExecContext(ctx(), "DROP INDEX scope_bindings_one_live_owner"); err != nil {
					t.Fatal(err)
				}
				if _, err := r.Store.DB.ExecContext(ctx(), "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('second-owner', 'parent', 'project', 'P1', 'owner-b', ?, 'active', 1, ?, ?)", host, fakeISO, fakeISO); err != nil {
					t.Fatal(err)
				}
			},
			reason: "duplicate_scope_owner", detail: "project 'P1' has more than one live owner ('owner-a', 'owner-b'), so there is no owner to write under. The reading paths report this and the writing paths refuse it; repair the store rather than letting one of them win",
			conflictKind: "project", conflictKey: "P1", incumbent: "owner-a", challenger: parent,
		},
		{
			name: "the issue is already scoped to another project", project: "P2",
			setup: func(t *testing.T, r *Registry) {
				bindProject(t, r, "P1", "first-parent")
				bindProject(t, r, "P2", parent)
				first := fixture()
				first.Parent = Endpoint{"first-parent", host, ns("/parent"), ns("cxc-first-parent")}
				first.ProjectKey = "P1"
				if _, err := r.Register(ctx(), first); err != nil {
					t.Fatal(err)
				}
			},
			reason: "foreign_scope", detail: "issue '" + issue + "' is already scoped to project 'P1' through another assignment, so it cannot also belong to 'P2'",
			conflictKind: "issue", conflictKey: issue, incumbent: "P1", challenger: "P2",
		},
		{
			name: "the project key is not an exact identifier", project: "a|b",
			reason: "unregistered_scope", detail: "a project key must not contain '|', which is the field separator",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRegistry(t)
			if c.setup != nil {
				c.setup(t, r)
			}
			conflicts := countRows(t, r, "SELECT * FROM linkage_conflicts")
			journaled := countRows(t, r, "SELECT * FROM journal WHERE kind = 'linkage_refused'")
			reason, detail := refusalOf(t, r.PrecheckScope(ctx(), parent, issue, c.project))
			if reason != c.reason || detail != c.detail {
				t.Fatalf("refused as %q %q", reason, detail)
			}
			wantRows := 0
			if c.conflictKind != "" {
				wantRows = 1
				rows, err := r.Conflicts(ctx(), c.conflictKind, c.conflictKey)
				if err != nil || len(rows) != 1 {
					t.Fatalf("conflict rows %v %v", rows, err)
				}
				row, ok := rows[0].(contract.OrderedObject)
				if !ok {
					t.Fatalf("conflict row %T", rows[0])
				}
				for key, want := range map[string]string{"reason": c.reason, "detail": c.detail, "challenger": c.challenger, "scopeKind": c.conflictKind, "scopeKey": c.conflictKey} {
					if got, _ := row.Lookup(key); got != want {
						t.Fatalf("conflict row %s is %v, want %q", key, got, want)
					}
				}
				if got, _ := row.Lookup("incumbent"); (c.incumbent == "") != (got == nil) || (c.incumbent != "" && got != c.incumbent) {
					t.Fatalf("conflict row incumbent is %v, want %q", got, c.incumbent)
				}
			}
			if after := countRows(t, r, "SELECT * FROM linkage_conflicts"); after != conflicts+wantRows {
				t.Fatalf("conflict rows %d -> %d", conflicts, after)
			}
			if after := countRows(t, r, "SELECT * FROM journal WHERE kind = 'linkage_refused'"); after != journaled+wantRows {
				t.Fatalf("linkage_refused entries %d -> %d", journaled, after)
			}
		})
	}
}

// Register, asked for a child that does exist, refuses the same two cases with the same words.
func TestPrecheckScope_AgreesWithRegister(t *testing.T) {
	for _, c := range []struct {
		name  string
		setup func(*testing.T, *Registry)
	}{
		{"unbound", nil},
		{"held by another parent", func(t *testing.T, r *Registry) { bindProject(t, r, "P1", "other-parent") }},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := newRegistry(t)
			if c.setup != nil {
				c.setup(t, r)
			}
			early, earlyDetail := refusalOf(t, r.PrecheckScope(ctx(), parent, issue, "P1"))
			in := fixture()
			in.ProjectKey = "P1"
			_, err := r.Register(ctx(), in)
			late, lateDetail := refusalOf(t, err)
			if early != late || earlyDetail != lateDetail {
				t.Fatalf("early %q %q, Register %q %q", early, earlyDetail, late, lateDetail)
			}
		})
	}
}

// A request the registry would take is not refused, and asking writes nothing: not for a bound
// project, not for a request naming no project, and not for the project that already holds the issue.
func TestPrecheckScope_CleanAskWritesNothing(t *testing.T) {
	r := newRegistry(t)
	bindProject(t, r, "P1", parent)
	tables := []string{"linkage_conflicts", "journal", "scope_bindings", "scope_links", "relationships", "relationship_scope"}
	snapshot := func() map[string]int {
		counts := map[string]int{}
		for _, table := range tables {
			counts[table] = countRows(t, r, "SELECT * FROM "+table)
		}
		return counts
	}
	unchanged := func(what string, before map[string]int) {
		t.Helper()
		after := snapshot()
		for _, table := range tables {
			if after[table] != before[table] {
				t.Fatalf("%s wrote %s: %d -> %d rows", what, table, before[table], after[table])
			}
		}
	}
	before := snapshot()
	if err := r.PrecheckScope(ctx(), parent, issue, "P1"); err != nil {
		t.Fatalf("a bound project was refused: %v", err)
	}
	unchanged("asking about a bound project", before)
	if err := r.PrecheckScope(ctx(), parent, issue, ""); err != nil {
		t.Fatalf("a request with no project was refused: %v", err)
	}
	unchanged("asking with no project", before)
	// The same project and issue already registered is the state a replay finds.
	in := fixture()
	in.ProjectKey = "P1"
	if _, err := r.Register(ctx(), in); err != nil {
		t.Fatal(err)
	}
	before = snapshot()
	if err := r.PrecheckScope(ctx(), parent, issue, "P1"); err != nil {
		t.Fatalf("the project that already holds the issue was refused: %v", err)
	}
	unchanged("asking about the project that holds the issue", before)
}
