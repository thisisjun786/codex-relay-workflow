package capacity

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// The store seat: the task registered as the relay's store-scope supervisor is the one that
// states the global ceiling. The seat is made by the built linkage-bind line, so this fails
// until that line accepts --scope-kind store.
const storeSeatTask = "01management"

// bindStoreSeat registers the store seat in this env's store through the relay's own command.
func bindStoreSeat(t *testing.T, e *env) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	line := []string{"--state", filepath.Dir(e.store.Path), "linkage-bind", "--role", "supervisor",
		"--scope-kind", "store", "--scope", "store", "--task", storeSeatTask, "--host", "host-s"}
	if code := dispatch.Execute(ctx(), "codex-session-relay", line, &stdout, &stderr); code != 0 {
		t.Fatalf("binding the store seat: exit %d stdout %q stderr %q", code, stdout.String(), stderr.String())
	}
}

// storeSeatLimit is the global ceiling the management task states.
func storeSeatLimit() Limit {
	return Limit{ScopeKind: "store", ScopeKey: "store", Dimension: "runs", Unit: "runs",
		Ceiling: 40, DeclaredBy: storeSeatTask, Source: "operator", Enforce: true}
}

// TestStoreSeatLimitDeclareRefusedWithoutTheSeat: the store scope has no owner of its own, so a
// task holding no live supervisor binding cannot state a global ceiling.
func TestStoreSeatLimitDeclareRefusedWithoutTheSeat(t *testing.T) {
	e := newEnv(t)
	if _, err := e.cap.DeclareLimit(ctx(), storeSeatLimit()); reasonOf(err) != string(contract.RefusalScopeRoleMismatch) {
		t.Fatalf("reason %q, want scope_role_mismatch (%v)", reasonOf(err), err)
	}
	rows, err := e.store.ExecutionLimits(ctx(), "store", "store")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("a refused declaration left %d execution_limits rows", len(rows))
	}
}

// TestStoreSeatLimitDeclareSucceedsWithTheSeat: once the store seat exists, the management task
// can state the store-wide ceiling and the row it wrote is readable.
func TestStoreSeatLimitDeclareSucceedsWithTheSeat(t *testing.T) {
	e := newEnv(t)
	bindStoreSeat(t, e)
	answer, err := e.cap.DeclareLimit(ctx(), storeSeatLimit())
	if err != nil {
		t.Fatal(err)
	}
	if got := field(answer, "dimension"); got != "runs" {
		t.Fatalf("dimension %v", got)
	}
	if got := field(answer, "declaredBy"); got != storeSeatTask {
		t.Fatalf("declaredBy %v", got)
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
