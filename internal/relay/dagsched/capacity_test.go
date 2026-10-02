package dagsched

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (f *fixture) capacityNow() Capacity {
	f.t.Helper()
	ctx := context.Background()
	c, err := f.sched.capacity(ctx, f.s.Q(ctx), "P-TEST")
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

// D-05: the DAG never runs more children than the standing cap of 6 unless a cap basis documents a larger ceiling. The clamp is DAG policy
// (capacity.Reserve knows nothing of it), so the reader decides it from the declared limit and the basis table.
func TestCapacityClamp(t *testing.T) {
	t.Run("no limit declared: the standing cap", func(t *testing.T) {
		f := newFixture(t)
		if c := f.capacityNow(); c.Ceiling != 6 || c.Source != "standing_cap" || c.Free != 6 {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("a limit above the cap without a basis is clamped", func(t *testing.T) {
		f := newFixture(t)
		f.declareLimit("project", "P-TEST", "runs", 10)
		if c := f.capacityNow(); c.Ceiling != 6 || c.Source != "clamped_no_basis" || c.Basis != "missing" || c.Free != 6 {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("the same limit with a recorded basis stands", func(t *testing.T) {
		f := newFixture(t)
		f.declareLimit("project", "P-TEST", "runs", 10)
		f.exec("INSERT INTO dag_cap_basis (limit_id, limit_revision, w_minutes, w_source, s_minutes, s_source, decided_by, decided_at) VALUES ('lim-project-runs', 1, 30, 'measured', 5, 'measured', 'owner', 't')")
		if c := f.capacityNow(); c.Ceiling != 10 || c.Source != "declared" || c.Basis != "recorded" || c.Free != 10 {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("a basis for another revision of the limit does not cover this one", func(t *testing.T) {
		f := newFixture(t)
		f.declareLimit("project", "P-TEST", "runs", 10)
		f.exec("INSERT INTO dag_cap_basis (limit_id, limit_revision, w_minutes, w_source, s_minutes, s_source, decided_by, decided_at) VALUES ('lim-project-runs', 2, 30, 'measured', 5, 'measured', 'owner', 't')")
		if c := f.capacityNow(); c.Ceiling != 6 || c.Source != "clamped_no_basis" {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("a limit below the cap is honoured and held slots count against it", func(t *testing.T) {
		f := newFixture(t)
		f.declareLimit("project", "P-TEST", "runs", 3)
		f.holdSlots(2)
		if c := f.capacityNow(); c.Ceiling != 3 || c.Held != 2 || c.Free != 1 || c.Source != "declared" {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("six held slots leave nothing under the standing cap", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("c", 0, "c-r1", addNode("n", dag.NodeNonPR))
		f.holdSlots(6)
		reading := f.read("c")
		if n := reading.node("n"); n.Reason != DeferNoCapacity || reading.Pass.FreeSlots != 0 || reading.Pass.Held != 6 {
			t.Fatalf("n = %+v pass = %+v, want defer:no_capacity at 6 held", n, reading.Pass)
		}
	})
	t.Run("the store scope binds when it is tighter than the project's", func(t *testing.T) {
		f := newFixture(t)
		f.declareLimit("store", "store", "runs", 1)
		f.holdSlots(1)
		if c := f.capacityNow(); c.Free != 0 || c.Ceiling != 1 {
			t.Fatalf("capacity = %+v", c)
		}
	})
	t.Run("an enforced dimension nobody measured leaves no slot free", func(t *testing.T) {
		f := newFixture(t)
		f.declareLimit("project", "P-TEST", "tokens", 1000)
		if c := f.capacityNow(); c.Free != 0 || !c.Unmeasured {
			t.Fatalf("capacity = %+v", c)
		}
		if err := f.s.ObserveExecutionUsage(context.Background(), store.ExecutionUsageRow{ScopeKind: "project", ScopeKey: "P-TEST", Dimension: "tokens", Observed: 400, ObservedBy: "parent", Method: "test", ObservedAt: "t"}); err != nil {
			t.Fatal(err)
		}
		if c := f.capacityNow(); c.Free != 6 || c.Unmeasured {
			t.Fatalf("measured and under the ceiling: capacity = %+v", c)
		}
		if err := f.s.ObserveExecutionUsage(context.Background(), store.ExecutionUsageRow{ScopeKind: "project", ScopeKey: "P-TEST", Dimension: "tokens", Observed: 1000, ObservedBy: "parent", Method: "test", ObservedAt: "t"}); err != nil {
			t.Fatal(err)
		}
		if c := f.capacityNow(); c.Free != 0 {
			t.Fatalf("observed at the ceiling: capacity = %+v", c)
		}
	})
}
