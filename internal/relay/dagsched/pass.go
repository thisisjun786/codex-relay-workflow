package dagsched

import (
	"context"
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
)

func (s *Scheduler) now() string {
	if s.Now != nil {
		return s.Now()
	}
	return registry.SystemISO()
}

// RecordPass reads the ready set and keeps the reading as a row of dag_passes (dag-ready --record): which nodes were ready, how many slots were free and
// which limit decided the order of the candidates it cut. The reading and the insert share one transaction, so a pass records what it saw. A pass is a
// fact and never a decision: a duplicate wake records another pass, and the release path does not read this table.
func (s *Scheduler) RecordPass(ctx context.Context, plan, actor string, opts ReadyOptions) (Reading, int64, error) {
	var reading Reading
	var seq int64
	err := s.Store.Compose(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		var err error
		if reading, err = s.Ready(txCtx, q, plan, opts); err != nil {
			return err
		}
		var last sql.NullInt64
		if err := q.QueryRowContext(txCtx, "SELECT MAX(pass_seq) FROM dag_passes WHERE plan_id = ?", plan).Scan(&last); err != nil {
			return err
		}
		seq = last.Int64 + 1
		_, err = q.ExecContext(txCtx, "INSERT INTO dag_passes (plan_id, pass_seq, plan_revision, input_digest, ready_count, free_slots, ceiling, held, deciding_limit,"+
			" order_json, dispositions_json, recorded_by, recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)",
			plan, seq, reading.PlanRevision, reading.InputDigest, reading.Pass.ReadyCount, reading.Pass.FreeSlots, reading.Pass.Ceiling, reading.Pass.Held,
			storedLimit(reading.Pass.DecidingLimit), reading.orderJSON(), reading.dispositionsJSON(), actor, s.now())
		if err != nil {
			return err
		}
		// the host the pass saw (CRW-468): the verdict, the values and the limit the reading decided by, which dag_passes cannot hold when it is the host's
		if h := reading.Pass.HostMemory; h != nil {
			if _, err = q.ExecContext(txCtx, "INSERT INTO dag_pass_host_memory (plan_id, pass_seq, state, reading_limit, host_json) VALUES (?,?,?,?,?)",
				plan, seq, h.State, reading.Pass.DecidingLimit, dag.Canonical(h.object())); err != nil {
				return err
			}
		}
		if reading.ReleasePolicy == nil {
			return nil
		}
		// the policy state this pass saw: the switch and its basis ride with the pass (CRW-411). A plan with no policy leaves the pass row alone.
		_, err = q.ExecContext(txCtx, "INSERT INTO dag_pass_release_policy (plan_id, pass_seq, policy_json) VALUES (?,?,?)", plan, seq, dag.Canonical(reading.ReleasePolicy.canonical()))
		return err
	})
	return reading, seq, err
}

// storedLimit is the limit dag_passes keeps for a reading's: its deciding_limit has a shipped closed set of four, so a pass the host memory bound cut is stored as the capacity cut it is and
// dag_pass_host_memory.reading_limit keeps the reading's own.
func storedLimit(limit string) string {
	if limit == LimitHostMemory {
		return LimitNoCapacity
	}
	return limit
}

// orderJSON is the ready node ids in release order.
func (r Reading) orderJSON() string {
	ids := make([]any, len(r.Ready))
	for i, n := range r.Ready {
		ids[i] = n.NodeID
	}
	return dag.Canonical(ids)
}

// dispositionsJSON is the reading's node list as a pass keeps it.
func (r Reading) dispositionsJSON() string {
	nodes := make([]any, len(r.Nodes))
	for i, n := range r.Nodes {
		node := map[string]any{"node_id": n.NodeID, "state": n.State, "disposition": n.Disposition, "reason": n.Reason, "detail": n.Detail}
		if n.Lifecycle != "" {
			node["lifecycle"] = n.Lifecycle
		}
		if n.Release != nil {
			node["release"] = n.Release.canonical()
		}
		if n.MergeOrder != nil {
			node["merge_order"] = n.MergeOrder.canonical()
		}
		nodes[i] = node
	}
	return dag.Canonical(nodes)
}
