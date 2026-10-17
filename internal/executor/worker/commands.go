package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"os"
	"syscall"
)

func (w *Worker) sealTerminal(ctx context.Context, id, state, proof string) (whole bool, err error) {
	err = w.transaction(ctx, func(c *sql.Conn) error {
		var live int
		if err := c.QueryRowContext(ctx, "SELECT COUNT(*) FROM commands WHERE execution_id=? AND state!='terminated'", id).Scan(&live); err != nil {
			return err
		}
		if live != 0 {
			return nil
		}
		_, err := c.ExecContext(ctx, "UPDATE starts SET state=?,proof=? WHERE execution_id=?", state, proof, id)
		whole = err == nil
		return err
	})
	return
}

func (w *Worker) RunGroup(ctx context.Context, g Group, runner GroupRunner) (out CommandResult, err error) {
	if err = w.AuthorizeGroup(ctx, g); err != nil {
		return out, err
	}
	replay := false
	err = w.transaction(ctx, func(c *sql.Conn) error {
		var raw, state, result string
		e := c.QueryRowContext(ctx, "SELECT group_json,state,result_json FROM commands WHERE permit_id=?", g.Permit.ID).Scan(&raw, &state, &result)
		if e == nil {
			if raw != jsonText(g) {
				return errors.New("command identity changed")
			}
			if state != "terminated" {
				return errors.New("command already started; reconcile before another request")
			}
			replay = true
			return json.Unmarshal([]byte(result), &out)
		}
		if !errors.Is(e, sql.ErrNoRows) {
			return e
		}
		if err := c.QueryRowContext(ctx, "SELECT state FROM starts WHERE execution_id=?", g.Reference.Identity.ExecutionID).Scan(&state); err != nil {
			return err
		}
		if state != "running" {
			return errors.New("execution is cancelling or terminal")
		}
		_, err := c.ExecContext(ctx, "INSERT INTO commands(permit_id,execution_id,group_json,owner_pid,state) VALUES (?,?,?,?,'running')", g.Permit.ID, g.Reference.Identity.ExecutionID, jsonText(g), os.Getpid())
		return err
	})
	if err != nil || replay {
		return out, err
	}
	out, err = runner.Run(ctx, g)
	if out.Terminated && out.Proof != "" {
		_, saveErr := w.db.ExecContext(context.WithoutCancel(ctx), "UPDATE commands SET state='terminated',result_json=? WHERE permit_id=?", jsonText(out), g.Permit.ID)
		err = errors.Join(err, saveErr)
	}
	return out, err
}

type GroupReconciler interface {
	InspectGroup(context.Context, Group, bool) (CommandResult, error)
}

// Only a dead supervisor's empty cgroup can settle an interrupted command. An
// empty cgroup while its launcher lives may still be just before exec.
func (w *Worker) reconcileGroups(ctx context.Context, id string, r GroupReconciler, cancel bool) error {
	rows, err := w.db.QueryContext(ctx, "SELECT group_json,owner_pid FROM commands WHERE execution_id=? AND state!='terminated'", id)
	if err != nil {
		return err
	}
	type pending struct {
		g   Group
		pid int
	}
	var groups []pending
	for rows.Next() {
		var raw string
		var p pending
		if err = rows.Scan(&raw, &p.pid); err != nil {
			break
		}
		if err = json.Unmarshal([]byte(raw), &p.g); err != nil {
			break
		}
		groups = append(groups, p)
	}
	if err = errors.Join(err, rows.Err(), rows.Close()); err != nil {
		return err
	}
	for _, p := range groups {
		live := syscall.Kill(p.pid, 0) != syscall.ESRCH
		if live && !cancel {
			continue
		}
		out, e := r.InspectGroup(ctx, p.g, cancel)
		if e != nil {
			continue
		}
		if !live && out.Terminated && out.Proof != "" {
			if _, err = w.db.ExecContext(ctx, "UPDATE commands SET state='terminated',result_json=? WHERE permit_id=?", jsonText(out), p.g.Permit.ID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Worker) ReconcileExecution(ctx context.Context, r executor.Reference, g *executor.Grant, runner GroupReconciler, cancel bool) error {
	if _, _, _, err := w.read(ctx, r); err != nil {
		return err
	}
	if cancel {
		if g == nil {
			return errors.New("cancellation authority required")
		}
		if err := w.RenewGrant(ctx, r, *g); err != nil {
			return err
		}
	}
	return w.reconcileGroups(ctx, r.Identity.ExecutionID, runner, cancel)
}
