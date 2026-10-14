package mergeturn

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport/storeseed"
)

// CRW-1449: the refusals a task gets for claiming a merge turn on a project it does not hold, and for a grant addressed
// to another task, name the layer, 자식 for the relay role id parent, and never the bare role id (layername.Find, the
// matcher the ci ratchet shares). The project sentences are the D2 shared ones capacity and dagsched write; the
// refusal codes stay.
func TestMergeTurnRefusalNamesLayers(t *testing.T) {
	refused := func(t *testing.T, err error, reason string) string {
		t.Helper()
		var r *store.RefusedError
		if !errors.As(err, &r) || r.Reason != reason {
			t.Fatalf("got %v, want a %s refusal", err, reason)
		}
		return r.Detail
	}
	check := func(t *testing.T, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("refusal = %q, want %q", got, want)
		}
		if hits := layername.Find(got); len(hits) != 0 {
			t.Errorf("%q names a bare role id: %v", got, hits)
		}
	}

	t.Run("project with no registered 자식", func(t *testing.T) {
		w := newFx(t)
		_, err := w.claimOn(alpha, "PRJ-NONE", "head-a", fxBase, true)
		check(t, refused(t, err, "unregistered_scope"), "project 'PRJ-NONE' has no registered 자식, so nobody can claim a merge turn for it")
	})

	t.Run("project with more than one live 자식", func(t *testing.T) {
		w := newFx(t)
		if _, err := w.s.DB.ExecContext(w.ctx, "DROP INDEX scope_bindings_one_live_owner"); err != nil {
			t.Fatal(err)
		}
		if err := storeseed.InsertScopeBinding(w.ctx, w.s, store.ScopeBindingsRow{BindingID: "bind-second-parent", Role: "parent",
			ScopeKind: "project", ScopeKey: fxA, TaskID: "task-gamma", HostID: "host-g", Status: "active", Revision: 1,
			CreatedAt: fxISO, UpdatedAt: fxISO}); err != nil {
			t.Fatal(err)
		}
		_, err := w.claimOn(alpha, fxA, "head-a", fxBase, true)
		check(t, refused(t, err, "duplicate_scope_owner"), "project 'PRJ-A' has more than one live 자식 ('task-alpha', 'task-gamma'), so there is no owner to hold a merge turn under; repair the store rather than letting one of them win")
	})

	t.Run("task that is not the registered 자식", func(t *testing.T) {
		w := newFx(t)
		_, err := w.claimOn(beta, fxA, "head-b", fxBase, true)
		check(t, refused(t, err, "scope_role_mismatch"), "task 'task-beta' is not the registered 자식 of project 'PRJ-A', which is held by 'task-alpha'; holding a merge turn is the project 자식's, and saying it is your turn is not being its 자식")
	})

	t.Run("grant addressed to another 자식", func(t *testing.T) {
		w, d, r := queueFixture(t)
		got, event, err := d.Channel(w.ctx, sql.NullString{String: r, Valid: true}, beta.TaskID, "grant", fxA)
		if err != nil || event != "" {
			t.Fatal(event, err)
		}
		check(t, got, "assignment 'rel-a' is addressed to 자식 'task-alpha', not to 'task-beta'")
	})
}
