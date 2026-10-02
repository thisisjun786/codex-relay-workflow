package dagsched

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// recordPass keeps a pass and returns it with its sequence number.
func (f *fixture) recordPass(plan string) (Reading, int64) {
	f.t.Helper()
	reading, seq, err := f.sched.RecordPass(contextBackground(), plan, "parent", ReadyOptions{})
	if err != nil {
		f.t.Fatal(err)
	}
	return reading, seq
}

type passRow struct {
	seq                                int64
	revision                           int64
	digest, limit, order, dispositions string
	ready, free, ceiling, held         int
	by                                 string
}

func (f *fixture) passRows(plan string) []passRow {
	f.t.Helper()
	rows, err := f.s.DB.QueryContext(contextBackground(), "SELECT pass_seq, plan_revision, input_digest, deciding_limit, order_json, dispositions_json, ready_count, free_slots, ceiling, held, recorded_by FROM dag_passes WHERE plan_id = ? ORDER BY pass_seq", plan)
	if err != nil {
		f.t.Fatal(err)
	}
	defer rows.Close()
	var out []passRow
	for rows.Next() {
		var r passRow
		if err := rows.Scan(&r.seq, &r.revision, &r.digest, &r.limit, &r.order, &r.dispositions, &r.ready, &r.free, &r.ceiling, &r.held, &r.by); err != nil {
			f.t.Fatal(err)
		}
		out = append(out, r)
	}
	return out
}

// Criterion c2: every pass records which limit decided it. Each row is one limit, read back from the table and not from the returned reading.
func TestPassRecordsDecidingLimit(t *testing.T) {
	t.Run("everything fits, then a ceiling cuts the third", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("pp", 0, "pp-r1", addNode("n1", dag.NodeNonPR), addNode("n2", dag.NodeNonPR), addNode("n3", dag.NodeNonPR))
		first, seq := f.recordPass("pp")
		if seq != 1 || first.Pass.DecidingLimit != LimitNone || first.Pass.ReadyCount != 3 {
			t.Fatalf("pass 1 = %d %+v", seq, first.Pass)
		}
		f.declareLimit("project", "P-TEST", "runs", 1)
		second, seq := f.recordPass("pp")
		if seq != 2 || second.Pass.DecidingLimit != LimitNoCapacity || second.Pass.ReadyCount != 1 {
			t.Fatalf("pass 2 = %d %+v", seq, second.Pass)
		}
		rows := f.passRows("pp")
		if len(rows) != 2 || rows[0].limit != "none" || rows[1].limit != "no_capacity" {
			t.Fatalf("rows = %+v", rows)
		}
		if rows[1].order != "[\"n1\"]" || rows[1].ready != 1 || rows[1].free != 1 || rows[1].ceiling != 1 || rows[1].held != 0 || rows[1].by != "parent" {
			t.Fatalf("pass 2 row = %+v", rows[1])
		}
		if rows[1].dispositions != second.dispositionsJSON() || rows[1].digest != second.InputDigest || rows[1].revision != second.PlanRevision {
			t.Fatal("the stored pass is not the reading it was taken from")
		}
		if rows[0].digest == rows[1].digest {
			t.Fatal("two readings that differ share a digest")
		}
		// a duplicate wake is another fact, not a replay
		f.recordPass("pp")
		if got := f.passRows("pp"); len(got) != 3 || got[2].seq != 3 {
			t.Fatalf("a third pass = %+v", got)
		}
	})
	t.Run("overlapping candidates", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("po", 0, "po-r1", addNode("i1", dag.NodeImplementation), addNode("i2", dag.NodeImplementation))
		f.declare("po", "i1", "a.go")
		f.declare("po", "i2", "a.go")
		reading, seq := f.recordPass("po")
		if seq != 1 || reading.Pass.DecidingLimit != LimitEditOverlap || reading.Pass.ReadyCount != 1 || f.passRows("po")[0].limit != "edit_overlap" {
			t.Fatalf("pass = %d %+v rows %+v", seq, reading.Pass, f.passRows("po"))
		}
	})
	t.Run("an enforced dimension nobody measured", func(t *testing.T) {
		f := newFixture(t)
		f.projectParent()
		f.putPlan("pt", 0, "pt-r1", addNode("n1", dag.NodeNonPR))
		f.declareLimit("project", "P-TEST", "tokens", 1000)
		reading, _ := f.recordPass("pt")
		if reading.Pass.DecidingLimit != LimitCapacityUnmeasured || reading.node("n1").Reason != DeferCapacityUnmeasured || f.passRows("pt")[0].limit != "capacity_unmeasured" {
			t.Fatalf("pass = %+v n1 = %+v", reading.Pass, reading.node("n1"))
		}
	})
	t.Run("a refused reading records nothing", func(t *testing.T) {
		f := newFixture(t)
		if _, _, err := f.sched.RecordPass(contextBackground(), "missing", "parent", ReadyOptions{}); err == nil {
			t.Fatal("a pass of a plan that does not exist was recorded")
		}
		if f.count("SELECT COUNT(*) FROM dag_passes") != 0 {
			t.Fatal("a refused reading left a pass")
		}
	})
}
