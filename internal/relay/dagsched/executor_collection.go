package dagsched

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

// RecordExecutorCollection takes an authenticated registry receipt and actual
// controller Git observations. It grants no acceptance or integration authority.
func (s *Scheduler) RecordExecutorCollection(ctx context.Context, id, actor string, c executor.CollectedExecution) error {
	if err := c.Inspect(ctx); err != nil {
		return err
	}
	return s.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		bound, err := s.ExecutorCheckContext(ctx, id, actor)
		if err != nil {
			return err
		}
		if bound.Admission.TaskDigest != c.Admission.TaskDigest {
			return errors.New("collected task differs from current assignment")
		}
		c.ExecutionID = c.Admission.Task.Identity.ExecutionID
		raw, err := json.Marshal(c)
		if err != nil {
			return err
		}
		var prior string
		err = s.Store.Q(ctx).QueryRowContext(ctx, `SELECT collection_json FROM executor_collections WHERE execution_id=?`, c.ExecutionID).Scan(&prior)
		if err == nil {
			if prior != string(raw) {
				return errors.New("execution already has another collected candidate")
			}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		_, err = s.Store.Q(ctx).ExecContext(ctx, `INSERT INTO executor_collections(execution_id,task_digest,result_digest,collection_json) VALUES (?,?,?,?)`, c.ExecutionID, c.Admission.TaskDigest, c.Receipt.ResultDigest, string(raw))
		return err
	})
}

// CollectedExecutor rechecks ownership/revision and reads the recorded checkout;
// callers use it before real review/verification and acceptance reads it again.
func (s *Scheduler) CollectedExecutor(ctx context.Context, id, actor string) (executor.CollectedExecution, error) {
	var c executor.CollectedExecution
	bound, err := s.ExecutorCheckContext(ctx, id, actor)
	if err != nil {
		return c, err
	}
	var raw string
	err = s.Store.Q(ctx).QueryRowContext(ctx, `SELECT collection_json FROM executor_collections WHERE execution_id=?`, bound.Admission.Task.Identity.ExecutionID).Scan(&raw)
	if err != nil {
		return c, err
	}
	if err = json.Unmarshal([]byte(raw), &c); err != nil {
		return c, err
	}
	if c.Admission.TaskDigest != bound.Admission.TaskDigest {
		return c, errors.New("stored collection task changed")
	}
	current, live, err := currentAgentExecution(ctx, s.Store.Q(ctx), bound.Admission.Task.Identity.Plan, bound.Admission.Task.Identity.Node)
	if err != nil || !live {
		return c, errors.Join(err, errors.New("collected execution is no longer current"))
	}
	c.ExecutionID = current.ExecutionID
	return c, c.Inspect(ctx)
}

// externalCommitProof is used only for enrolled executions. The legacy execution
// worktree remains the source for every execution without an executor assignment.
func (s *Scheduler) externalCommitProof(ctx context.Context, e AgentExecution, in CommitRef) (AgentExecution, error) {
	_, err := s.ExecutorAssignment(ctx, e.ExecutionID)
	if errors.Is(err, sql.ErrNoRows) {
		return e, nil
	}
	if err != nil {
		return e, err
	}
	c, err := s.CollectedExecutor(ctx, e.ExecutionID, e.Actor)
	if err != nil {
		return e, err
	}
	if in.Checkout != c.Candidate.Checkout || in.Base != c.Candidate.Base || in.Head != c.Candidate.Head {
		return e, errors.New("acceptance differs from recorded controller collection")
	}
	raw, err := readVerificationRecord(in.Record)
	if err != nil {
		return e, err
	}
	record, err := DecodeVerificationRecord(raw)
	if err != nil {
		return e, err
	}
	if record.HeadCommit != c.Candidate.Head || record.BaseCommit != c.Candidate.Base {
		return e, errors.New("external acceptance requires fresh head/range verification")
	}
	// e is a local copy; no existing execution row is rewritten.
	e.Worktree = c.Candidate.Checkout
	return e, nil
}
