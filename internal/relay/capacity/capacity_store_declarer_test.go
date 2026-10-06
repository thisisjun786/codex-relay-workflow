package capacity

import (
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The store declarer: the store scope has no Linear level of its own, so the seat that may state
// its ceiling is the store-scope supervisor binding alone. An initiative-scope supervisor seat is
// a seat for that initiative, not for the store, and cannot state a store-wide bound (CRW-773).

// storeDeclarerDetail is the phrase the narrowed refusal carries.
const storeDeclarerDetail = "holds no live store-scope supervisor binding"

// storeDeclarerRefusalDetail is the refusal detail, or a fatal when the call did not refuse.
func storeDeclarerRefusalDetail(t *testing.T, err error) string {
	t.Helper()
	var refused *store.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("not a refusal: %v", err)
	}
	return refused.Detail
}

// TestStoreDeclarerInitiativeSeatIsRefused is the red test: the fixture's task-supervisor holds
// an initiative-scope supervisor binding for INIT-1 and no store seat, so a store-scope
// declaration by it is refused with scope_role_mismatch and nothing is written. Before the
// narrowing this declaration succeeded, because the store branch read any live supervisor
// binding.
func TestStoreDeclarerInitiativeSeatIsRefused(t *testing.T) {
	e := newEnv(t)
	answer, err := e.cap.DeclareLimit(ctx(), Limit{ScopeKind: "store", ScopeKey: "store", Dimension: "runs",
		Unit: "runs", Ceiling: 40, DeclaredBy: supervisor, Source: "operator", Enforce: true})
	if answer != nil {
		t.Fatalf("an initiative-scope supervisor declared a store ceiling: %v", answer)
	}
	if got := reasonOf(err); got != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("reason %q, want scope_role_mismatch (%v)", got, err)
	}
	if detail := storeDeclarerRefusalDetail(t, err); !strings.Contains(detail, storeDeclarerDetail) {
		t.Fatalf("detail %q does not carry %q", detail, storeDeclarerDetail)
	}
	rows, err := e.store.ExecutionLimits(ctx(), "store", "store")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused declaration left %d execution_limits rows", len(rows))
	}
}

// TestStoreDeclarerStoreSeatIsAccepted: the task holding the store-scope supervisor seat states
// the store-wide ceiling, and the row it wrote is readable.
func TestStoreDeclarerStoreSeatIsAccepted(t *testing.T) {
	e := newEnv(t)
	bindStoreSeat(t, e)
	answer, err := e.cap.DeclareLimit(ctx(), storeSeatLimit())
	if err != nil {
		t.Fatal(err)
	}
	if got := field(answer, "declaredBy"); got != storeSeatTask {
		t.Fatalf("declaredBy %v, want %q", got, storeSeatTask)
	}
	id, err := LimitID("store", "store", "runs")
	if err != nil {
		t.Fatal(err)
	}
	row, err := e.store.ExecutionLimit(ctx(), id)
	if err != nil {
		t.Fatal(err)
	}
	if row.ScopeKind != "store" || row.ScopeKey != "store" || row.Dimension != "runs" ||
		row.Ceiling != 40 || row.DeclaredBy != storeSeatTask || row.Enforce != 1 {
		t.Fatalf("execution_limits row %+v", row)
	}
}

// TestStoreDeclarerOtherScopesUnchanged: narrowing the store branch leaves the initiative and
// project branches as they were: the initiative's own supervisor and the project's parent still
// state their own scopes.
func TestStoreDeclarerOtherScopesUnchanged(t *testing.T) {
	e := newEnv(t)
	initiative := e.ceiling("runs", 9, "initiative", "INIT-1", "runs", true, "")
	project := e.ceiling("runs", 4, "project", projectA, "runs", true, "")
	if got := field(initiative, "declaredBy"); got != supervisor {
		t.Fatalf("initiative declaredBy %v, want %q", got, supervisor)
	}
	if got := field(project, "declaredBy"); got != alpha {
		t.Fatalf("project declaredBy %v, want %q", got, alpha)
	}
	room, err := e.cap.Headroom(ctx(), "initiative", "INIT-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := field(field(room, "dimensions").([]any)[0], "ceiling"); got != 9.0 {
		t.Fatalf("initiative ceiling %v", got)
	}
}

// TestStoreDeclarerInitiativeKeyedStoreIsRefused: the added conjunct is scope_kind, not the key.
// A supervisor whose binding sits on an initiative that happens to be keyed "store" holds a live
// supervisor binding whose scope_key reads "store", so the old predicate (which never named the
// kind) admitted it. The store seat is the store *kind*, so it is refused.
func TestStoreDeclarerInitiativeKeyedStoreIsRefused(t *testing.T) {
	e := newEnv(t)
	const keyed = "task-initiative-keyed-store"
	r := &registry.Registry{Store: e.store, Now: e.clock.iso}
	if _, err := r.BindScope(ctx(), "supervisor", "store", registry.Endpoint{TaskID: keyed, HostID: "host-s", Cwd: ns("/keyed")}); err != nil {
		t.Fatalf("binding an initiative keyed %q: %v", "store", err)
	}
	answer, err := e.cap.DeclareLimit(ctx(), Limit{ScopeKind: "store", ScopeKey: "store", Dimension: "runs",
		Unit: "runs", Ceiling: 40, DeclaredBy: keyed, Source: "operator", Enforce: true})
	if answer != nil {
		t.Fatalf("an initiative keyed %q declared a store ceiling: %v", "store", answer)
	}
	if got := reasonOf(err); got != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("reason %q, want scope_role_mismatch (%v)", got, err)
	}
	if detail := storeDeclarerRefusalDetail(t, err); !strings.Contains(detail, storeDeclarerDetail) {
		t.Fatalf("detail %q does not carry %q", detail, storeDeclarerDetail)
	}
	rows, err := e.store.ExecutionLimits(ctx(), "store", "store")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused declaration left %d execution_limits rows", len(rows))
	}
}
