package mergeturn

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/layername"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
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

// CRW-1449 (PR #890 crosscheck, CRW-1483): the refusals of taking a held turn away (lifecycle.go authority) and of passing a
// stalled one (pass.go) are composed at run time, the 부모 clause chosen by whether the project has a readable 부모 above it,
// so the per-literal ci scan never reads the sentence whole. Every form names the layers.
func TestTurnAuthorityRefusalsNameLayers(t *testing.T) {
	refused := func(t *testing.T, err error) string {
		t.Helper()
		var r *store.RefusedError
		if !errors.As(err, &r) || r.Reason != "scope_role_mismatch" {
			t.Fatalf("got %v, want a scope_role_mismatch refusal", err)
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
	// orphan binds a project with a 자식 and no 부모 above it.
	const orphanProject = "PRJ-ORPHAN"
	orphan := ep("task-orphan", "host-o", "/orphan")
	bindOrphan := func(w *fx) {
		if _, err := w.r.BindScope(w.ctx, "parent", orphanProject, orphan); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("take the turn away, project with a 부모", func(t *testing.T) {
		w := newFx(t)
		turn := w.held()
		_, err := w.m.Release(w.ctx, turn, "task-stranger", "cancelled", "stalled", "no progress")
		check(t, refused(t, err), "task 'task-stranger' is neither the holder of turn "+pyvalue.StrRepr(turn)+" nor the 부모 above project 'PRJ-A', which is 'task-supervisor', so it cannot take the turn away")
	})

	t.Run("take the turn away, project with no readable 부모", func(t *testing.T) {
		w := newFx(t)
		bindOrphan(w)
		turn := w.heldOn(orphan, orphanProject, "head-o", fxBase)
		_, err := w.m.Release(w.ctx, turn, "task-stranger", "cancelled", "stalled", "no progress")
		check(t, refused(t, err), "task 'task-stranger' is neither the holder of turn "+pyvalue.StrRepr(turn)+" nor the 부모 above project 'PRJ-ORPHAN', and that project has no readable 부모, so it cannot take the turn away")
	})

	t.Run("pass, project with a 부모", func(t *testing.T) {
		w, clock := clockedFx(t)
		turn := w.held()
		clock.advance(30 * time.Minute)
		_, err := w.m.Pass(w.ctx, turn, "task-stranger", "nobody")
		check(t, refused(t, err), "task 'task-stranger' is neither the 부모 above project 'PRJ-A', which is 'task-supervisor' nor the 자식 of a waiting claim on target "+pyvalue.StrRepr(fxTarget(t, w, turn))+" that still owns its project, so it cannot pass turn "+pyvalue.StrRepr(turn)+" on")
	})

	t.Run("pass, project with no readable 부모", func(t *testing.T) {
		w, clock := clockedFx(t)
		bindOrphan(w)
		turn := w.heldOn(orphan, orphanProject, "head-o", fxBase)
		clock.advance(30 * time.Minute)
		_, err := w.m.Pass(w.ctx, turn, "task-stranger", "nobody")
		check(t, refused(t, err), "task 'task-stranger' is neither the 부모 above project 'PRJ-ORPHAN', and that project has no readable 부모 nor the 자식 of a waiting claim on target "+pyvalue.StrRepr(fxTarget(t, w, turn))+" that still owns its project, so it cannot pass turn "+pyvalue.StrRepr(turn)+" on")
	})
}

// fxTarget is the target key of a turn, as its row records it.
func fxTarget(t *testing.T, w *fx, turn string) string {
	t.Helper()
	r, err := w.m.row(w.ctx, turn)
	if err != nil {
		t.Fatal(err)
	}
	return r.TargetKey
}
