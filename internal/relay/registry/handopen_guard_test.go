package registry

import (
	"context"
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1036. The guard a generation opened or bound by hand passes is asked inside the transaction that writes the generation or the
// binding, so a plan revision that lands between the check and the write cannot leave a generation the guard would have refused.
// These tests are not parallel: they swap the package's guard, and parallel tests start only after every serial one has finished.

func installGuard(t *testing.T, guard func(ctx context.Context, s *store.Store, relationship, reason string, generation int64) error) {
	t.Helper()
	was := HandOpenedGenerationGuard
	HandOpenedGenerationGuard = guard
	t.Cleanup(func() { HandOpenedGenerationGuard = was })
}

func TestTheGuardOfAHandOpenedGenerationRunsInsideTheWritingTransaction(t *testing.T) {
	r := newRegistry(t)
	in := fixture()
	in.DispatchTurnID = sql.NullString{}
	x, err := r.Register(ctx(), in)
	if err != nil {
		t.Fatal(err)
	}
	var calls []string
	installGuard(t, func(ctx context.Context, s *store.Store, relationship, reason string, generation int64) error {
		calls = append(calls, reason)
		if !s.InTransaction(ctx) {
			t.Errorf("the guard for generation %d ran outside the transaction that writes", generation)
		}
		return nil
	})
	if _, err := r.OpenGeneration(ctx(), x.ID, "dispatch-2", "needs_changes_revision", sql.NullString{}); err != nil {
		t.Fatal(err)
	}
	// a repeated open is a replay: it was checked when it was opened
	if _, err := r.OpenGeneration(ctx(), x.ID, "dispatch-2", "needs_changes_revision", sql.NullString{}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BindAnchor(ctx(), x.ID, 2, "turn-exact", "dispatch_receipt"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("the guard ran %d times (%v), want once for the open and once for the bind", len(calls), calls)
	}
}

// A guard that refuses leaves nothing behind: no generation is opened, no anchor is bound.
func TestARefusedHandOpenedGenerationWritesNothing(t *testing.T) {
	r := newRegistry(t)
	in := fixture()
	in.DispatchTurnID = sql.NullString{}
	x, err := r.Register(ctx(), in)
	if err != nil {
		t.Fatal(err)
	}
	installGuard(t, func(context.Context, *store.Store, string, string, int64) error {
		return refuse(contract.RefusalDispositionConflict, "no generation by hand")
	})
	if _, err := r.OpenGeneration(ctx(), x.ID, "dispatch-2", "needs_changes_revision", sql.NullString{}); err == nil || !strings.Contains(err.Error(), "no generation by hand") {
		t.Fatalf("open = %v, want the guard's refusal", err)
	}
	if _, err := r.BindAnchor(ctx(), x.ID, 1, "turn-exact", "dispatch_receipt"); err == nil || !strings.Contains(err.Error(), "no generation by hand") {
		t.Fatalf("bind = %v, want the guard's refusal", err)
	}
	got, err := r.Get(ctx(), x.ID)
	if err != nil || len(got.Generations) != 1 || got.Generations[0].AnchorState == AnchorBound {
		t.Fatalf("generations after refused writes = %+v (%v), want the first one still unbound", got.Generations, err)
	}
}
