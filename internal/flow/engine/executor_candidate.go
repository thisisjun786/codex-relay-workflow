package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// ExecutorCandidateResolver reads current controller authority and the durable
// collection. It must return nil for nodes that do not use external execution.
type ExecutorCandidateResolver func(context.Context, SnapshotRequest) (*ExecutorCandidateReview, error)

// Criteria comes from the controller's current plan input, not the worker's
// narrative. Its digest must match the current assignment before a real call.
type ExecutorCandidateReview struct {
	executor.CollectedExecution
	Criteria []delivery.Criterion
}

func (c *callRun) prepareExecutorCandidate() {
	p := c.plan
	if c.e.cfg.ExecutorCandidate == nil || p.node == "" || (p.role != "reviewer" && p.role != "reviewer-final") {
		return
	}
	bound, err := c.e.cfg.ExecutorCandidate(c.e.ctx, SnapshotRequest{RunID: c.e.runID, CallID: c.id, Node: p.node, Role: p.role, Head: p.opts.Head})
	if err == nil && bound == nil {
		return
	}
	if err == nil {
		if len(bound.Criteria) == 0 || delivery.SetDigest(bound.Criteria) != bound.Admission.Task.Identity.CriteriaDigest {
			err = errors.New("review requires the current criteria text, matching its registered digest")
		} else if bound.Admission.Task.Identity.Node != p.node || (p.opts.Head != "" && p.opts.Head != bound.Candidate.Head) || p.resume != nil {
			err = errors.New("external review requires a fresh call on the collected node and head")
		} else {
			err = bound.Inspect(c.e.ctx)
		}
	}
	if err != nil {
		c.prepHold = &Refusal{Reason: ReasonWorktreeFailed, Detail: err.Error()}
		return
	}
	copy := bound.CollectedExecution
	c.executorCandidate = &copy
	p.opts.Head, p.opts.Cwd = bound.Candidate.Head, bound.Candidate.Checkout
	c.cwd = bound.Candidate.Checkout
	p.prompt += fmt.Sprintf("\n\nController-collected candidate: %s\nBase: %s\nCurrent criteria digest: %s\nController assignment:\n%s", bound.Candidate.Head, bound.Candidate.Base, bound.Admission.Task.Identity.CriteriaDigest, bound.Admission.Task.Input.Prompt)
	criteria, _ := json.Marshal(bound.Criteria)
	p.prompt += "\nCurrent acceptance criteria:\n" + string(criteria)
}

func (c *callRun) executorCandidateStands(ctx context.Context) error {
	if c.executorCandidate == nil {
		return nil
	}
	bound, err := c.e.cfg.ExecutorCandidate(ctx, SnapshotRequest{RunID: c.e.runID, CallID: c.id, Node: c.plan.node, Role: c.plan.role, Head: c.plan.opts.Head})
	if err != nil {
		return err
	}
	old := c.executorCandidate
	if bound == nil || delivery.SetDigest(bound.Criteria) != old.Admission.Task.Identity.CriteriaDigest || bound.Admission.TaskDigest != old.Admission.TaskDigest || bound.ExecutionID != old.ExecutionID || bound.Receipt != old.Receipt || bound.Candidate.Head != old.Candidate.Head || bound.Candidate.Tree != old.Candidate.Tree || filepath.Clean(bound.Candidate.Checkout) != filepath.Clean(old.Candidate.Checkout) {
		return errors.New("external candidate authority changed during review")
	}
	return bound.Inspect(ctx)
}

type ExecutorCandidateAuthority interface {
	RecordExecutorCollection(context.Context, string, string, executor.CollectedExecution) error
	CollectedExecutor(context.Context, string, string) (executor.CollectedExecution, error)
}

// BindCandidate connects the authenticated registry delivery to CRW-1337's Git
// collection. The authority persists only collection provenance, never reviews.
func (c *ExecutorCollector) BindCandidate(ctx context.Context, id, actor string, in executor.ResultCheckInput, authority ExecutorCandidateAuthority) (executor.CollectedExecution, error) {
	var bound executor.CollectedExecution
	result, ack, err := c.Receipt(id)
	if err != nil {
		return bound, err
	}
	if authority == nil || result.TaskDigest != in.Admission.TaskDigest || result.Payload.Candidate == nil {
		return bound, errors.New("candidate binding needs current authority and a committed result")
	}
	if prior, e := authority.CollectedExecutor(ctx, id, actor); e == nil {
		if prior.Receipt != ack {
			return bound, errors.New("stored collection differs from authenticated delivery")
		}
		return prior, nil
	} else if !errors.Is(e, sql.ErrNoRows) {
		return bound, e
	}
	report := executor.CheckExecutorResult(ctx, in)
	if !report.EnvelopeValid || !report.CandidateValid || report.Candidate == nil || report.ResultDigest != ack.ResultDigest {
		return bound, errors.New("collection differs from authenticated registry delivery")
	}
	bound = executor.CollectedExecution{Admission: in.Admission, Candidate: *report.Candidate, Receipt: ack, ExecutionID: id}
	if err = authority.RecordExecutorCollection(ctx, id, actor, bound); err != nil {
		return bound, err
	}
	return authority.CollectedExecutor(ctx, id, actor)
}
