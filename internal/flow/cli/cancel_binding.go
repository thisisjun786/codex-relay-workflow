package cli

import (
	"context"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/registry"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (c *command) releaseCancelledBinding(st *store.Store, state, run string) error {
	spec, err := ReadSpec(state, run)
	if errors.Is(err, ErrNoSpec) {
		return nil
	}
	if err != nil {
		return err
	}
	if spec.Binding == nil {
		return nil
	} // old runs have no tenure evidence
	_, err = (&registry.Registry{Store: st}).ReleaseProjectBinding(c.ctx, *spec.Binding, run, func(ctx context.Context, q store.Querier) error {
		return otherRunsEnded(ctx, q, *spec.Binding, run)
	})
	return err
}

func otherRunsEnded(ctx context.Context, q store.Querier, b registry.ProjectBinding, run string) error {
	rows, err := q.QueryContext(ctx, `SELECT f.run_id, f.workflow, f.plan_id, f.plan_revision, f.actor_task_id
FROM flow_runs f JOIN dag_plans p ON p.plan_id=f.plan_id
WHERE p.project_key=? AND f.actor_task_id=? AND f.run_id<>?`, b.Project, b.Task, run)
	if err != nil {
		return err
	}
	var records []RunRecord
	for rows.Next() {
		var r RunRecord
		if err := rows.Scan(&r.RunID, &r.Workflow, &r.PlanID, &r.PlanRevision, &r.Owner); err != nil {
			_ = rows.Close()
			return err
		}
		records = append(records, r)
	}
	if err := errors.Join(rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, r := range records {
		events, err := journal.LoadLeanIn(ctx, q, r.RunID)
		if err != nil {
			return err
		}
		if info := Summarize(r, events, false, 0); !Over(info.State) {
			return fmt.Errorf("binding %s still owns other unfinished runs: %s is %s", b.ID, r.RunID, info.State)
		}
	}
	return nil
}
