package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ProjectBinding pins one tenure, not just a task's deterministic binding id.
// UpdatedAt also fences reactivation, which keeps the binding's revision.
type ProjectBinding struct {
	ID        string `json:"id"`
	Project   string `json:"project"`
	Task      string `json:"task"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
	PinSeq    int64  `json:"pin_seq"`
}

// PinProjectBinding records the start before its process is spawned. A later
// start therefore protects its tenure even before it writes a flow_runs row.
func (r *Registry) PinProjectBinding(ctx context.Context, project, task, run string) (*ProjectBinding, error) {
	var b *ProjectBinding
	err := r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		b, err = r.ProjectBinding(ctx, project, task)
		if err != nil || b == nil {
			return err
		}
		if err = r.Journal(ctx, "scope_run_pin", b.ID, map[string]any{"run": run, "project": project, "task": task}, r.now()); err != nil {
			return err
		}
		return r.Store.Q(ctx).QueryRowContext(ctx, "SELECT last_insert_rowid()").Scan(&b.PinSeq)
	})
	return b, err
}

func (r *Registry) ProjectBinding(ctx context.Context, project, task string) (*ProjectBinding, error) {
	b := &ProjectBinding{Project: project, Task: task}
	err := r.Store.Q(ctx).QueryRowContext(ctx, "SELECT binding_id, revision, updated_at FROM scope_bindings WHERE role='parent' AND scope_kind='project' AND scope_key=? AND task_id=? AND status IN ('active','paused') AND superseded_by IS NULL", project, task).Scan(&b.ID, &b.Revision, &b.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return b, err
}

// ReleaseProjectBinding retires only the tenure the cancelling run recorded.
// A changed binding is left alone. Other live work keeps its owner and yields an
// explicit error; the caller can retry once that work has ended.
func (r *Registry) ReleaseProjectBinding(ctx context.Context, b ProjectBinding, run string, idle func(context.Context, store.Querier) error) (bool, error) {
	if b.ID == "" || b.Project == "" || b.Task == "" || b.Revision < 1 || b.UpdatedAt == "" || run == "" {
		return false, fmt.Errorf("a binding release needs its recorded tenure and run")
	}
	released := false
	release, err := r.lockProjectParent(ctx, roleParent, b.Project)
	if err != nil {
		return false, err
	}
	defer release()
	err = r.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		q := r.Store.Q(ctx)
		var present int
		err := q.QueryRowContext(ctx, "SELECT 1 FROM scope_bindings WHERE binding_id=? AND role='parent' AND scope_kind='project' AND scope_key=? AND task_id=? AND revision=? AND updated_at=? AND status IN ('active','paused') AND superseded_by IS NULL", b.ID, b.Project, b.Task, b.Revision, b.UpdatedAt).Scan(&present)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var latest int64
		if err := q.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0) FROM journal WHERE kind='scope_run_pin' AND subject=?", b.ID).Scan(&latest); err != nil {
			return err
		}
		if latest != b.PinSeq {
			return nil
		} // another run started, even if it has not spawned yet
		var owns int
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM flow_runs f JOIN dag_plans p ON p.plan_id=f.plan_id
WHERE f.run_id=? AND f.actor_task_id=? AND p.project_key=?
AND EXISTS (SELECT 1 FROM flow_events e WHERE e.run_id=f.run_id AND e.kind='finished' AND json_extract(e.payload_json,'$.error_kind')='cancelled'
 AND e.seq>(SELECT COALESCE(MAX(s.seq),0) FROM flow_events s WHERE s.run_id=f.run_id AND s.kind='started'))`, run, b.Task, b.Project).Scan(&owns); err != nil {
			return err
		}
		if owns != 1 {
			return fmt.Errorf("run %s has no cancelled end under binding %s", run, b.ID)
		}
		var count int
		if err := q.QueryRowContext(ctx, "SELECT COUNT(*) FROM relationships r JOIN relationship_scope s ON s.relationship_id=r.relationship_id WHERE s.project_key=? AND r.parent_task_id=? AND r.status IN ('active','paused') AND r.superseded_by IS NULL", b.Project, b.Task).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("binding %s still owns %d live assignments", b.ID, count)
		}
		// The flow owner judges held, paused and otherwise resumable runs with its
		// canonical reader, in this transaction rather than by a second SQL state machine.
		if idle == nil {
			return fmt.Errorf("a binding release needs the run-state judgment")
		}
		if err := idle(ctx, q); err != nil {
			return err
		}
		// The DAG owns execution closure; never discard an execution by hiding its binding.
		if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM dag_agent_executions e JOIN dag_plans p ON p.plan_id=e.plan_id
WHERE p.project_key=? AND e.actor_task_id=?
AND NOT EXISTS (SELECT 1 FROM dag_agent_executions n WHERE n.supersedes_execution_id=e.execution_id)
AND NOT EXISTS (SELECT 1 FROM dag_agent_closures c WHERE c.subject_kind='execution' AND c.subject_id=e.execution_id)`, b.Project, b.Task).Scan(&count); err != nil {
			return err
		}
		if count > 0 {
			return fmt.Errorf("binding %s still owns %d open executions", b.ID, count)
		}
		now := r.now()
		if _, err := q.ExecContext(ctx, "UPDATE scope_bindings SET status='cancelled', revision=revision+1, updated_at=? WHERE binding_id=?", now, b.ID); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, "UPDATE scope_links SET status='archived', revision=revision+1, updated_at=? WHERE status IN ('active','paused') AND superseded_by IS NULL AND ((lower_kind='project' AND lower_key=? AND lower_task_id=?) OR (upper_kind='project' AND upper_key=? AND upper_task_id=?))", now, b.Project, b.Task, b.Project, b.Task); err != nil {
			return err
		}
		if err := r.Journal(ctx, "scope_released", b.ID, map[string]any{"run": run, "scopeKey": b.Project, "taskId": b.Task, "bindingRevision": b.Revision, "reason": "flow_cancel"}, now); err != nil {
			return err
		}
		released = true
		return nil
	})
	return released, err
}
