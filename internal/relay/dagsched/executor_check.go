package dagsched

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
)

// ExecutorCheckContext is read from the controller, never supplied by a result.
// Remote checkout roots are deliberately absent from the collection source.
type ExecutorCheckContext struct {
	Admission     executor.Admission
	Repository    executor.RegisteredRepository
	ExpectedTools []executor.Tool
}

func (s *Scheduler) ExecutorCheckContext(ctx context.Context, id, actor string) (ExecutorCheckContext, error) {
	var out ExecutorCheckContext
	a, err := s.ExecutorAssignment(ctx, id)
	if err != nil {
		return out, err
	}
	task := a.Admission.Task
	q := s.Store.Q(ctx)
	if err = s.fence(ctx, q, task.Identity.Plan, actor); err != nil {
		return out, err
	}
	snap, n, err := liveNode(ctx, q, task.Identity.Plan, task.Identity.Node)
	if err != nil {
		return out, err
	}
	if err = requireProjectParent(ctx, q, snap.ProjectKey, actor); err != nil {
		return out, err
	}
	cur, live, err := currentAgentExecution(ctx, q, task.Identity.Plan, task.Identity.Node)
	if err != nil {
		return out, err
	}
	stands, err := agentExecutionStands(ctx, q, cur, task.Identity.ExecutionID)
	if err != nil {
		return out, err
	}
	if !live || !stands || cur.Actor != actor {
		return out, errors.New("assignment is not the current owned execution")
	}
	if strconv.FormatInt(snap.Revision, 10) != task.Identity.Revision || n.CriteriaSetDigest != task.Identity.CriteriaDigest {
		return out, errors.New("assignment plan revision or criteria are stale")
	}
	pool := &capacity.HostPool{Store: s.Store}
	host, err := pool.Status(ctx, task.Executor.HostID)
	if err != nil {
		return out, err
	}
	if err = host.Host.Validate(); err != nil {
		return out, err
	}
	if _, ok := host.Host.Repositories[task.Repository.ID]; !ok || task.Repository.Forge != "github" {
		return out, errors.New("repository is not enrolled for this host/forge")
	}
	if host.Observation.Incarnation != task.Executor.Incarnation || host.Host.Kind != task.Executor.Kind || host.Host.CapabilityRevision != task.Executor.CapabilityRevision || host.Host.LaunchProfile != task.Executor.LaunchProfileDigest || host.Host.ToolchainProfile != task.Executor.ToolchainProfileDigest {
		return out, errors.New("host identity or profiles changed since assignment")
	}
	out.Admission = a.Admission
	out.Repository = executor.RegisteredRepository{Forge: task.Repository.Forge, ID: task.Repository.ID, Source: "https://github.com/" + task.Repository.ID + ".git"}
	rows, err := q.QueryContext(ctx, "SELECT manifest_json FROM executor_toolchain_plans WHERE host_id=? AND profile_digest=? AND state='applied'", task.Executor.HostID, task.Executor.ToolchainProfileDigest)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			return out, err
		}
		var m worker.Toolchain
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return out, err
		}
		if m.Digest() != task.Executor.ToolchainProfileDigest {
			return out, errors.New("stored tool manifest digest differs from the assignment")
		}
		out.ExpectedTools = nil
		for _, f := range m.Files {
			out.ExpectedTools = append(out.ExpectedTools, executor.Tool{Name: f.Path, Version: f.Version, SHA256: f.SHA256})
		}
	}
	return out, rows.Err()
}

// CheckExecutorGates invokes the existing read-side judgments on a controller
// checkout. It writes neither flow_calls nor acceptance rows and never invokes
// the result's command text. Acceptance must recheck authority at its own commit.
func (s *Scheduler) CheckExecutorGates(ctx context.Context, id executor.Identity, actor string, c *executor.CollectedCandidate, verification, premerge []byte) ([]executor.RequiredGate, error) {
	gates := []executor.RequiredGate{}
	need := func(name string, err error) {
		if err != nil {
			gates = append(gates, executor.RequiredGate{Gate: name, Reason: err.Error()})
		}
	}
	q := s.Store.Q(ctx)
	if err := s.fence(ctx, q, id.Plan, actor); err != nil {
		return nil, err
	}
	snap, n, err := liveNode(ctx, q, id.Plan, id.Node)
	if err != nil {
		return nil, err
	}
	if err = requireProjectParent(ctx, q, snap.ProjectKey, actor); err != nil {
		return nil, err
	}
	e, live, err := currentAgentExecution(ctx, q, id.Plan, id.Node)
	if err != nil {
		return nil, err
	}
	stands, err := agentExecutionStands(ctx, q, e, id.ExecutionID)
	if err != nil {
		return nil, err
	}
	if !live || !stands || e.Actor != actor {
		return nil, errors.New("candidate no longer belongs to the current owned execution")
	}
	if n.CriteriaSetDigest != id.CriteriaDigest {
		return nil, errors.New("current criteria differ from the frozen task")
	}
	head, tree := "", ""
	if c != nil {
		head, tree = c.Head, c.Tree
		keys, keyErr := CommitVerificationKeys(ctx, c.Checkout, c.Head)
		if keyErr == nil {
			keys.Base = c.Base
			keys.OS, keys.Arch = runtime.GOOS, runtime.GOARCH
			var record VerificationRecord
			record, keyErr = JudgeVerificationRecord(verification, keys)
			if keyErr == nil && record.HeadCommit != head {
				keyErr = fmt.Errorf("verification record names head %s; fresh head/range evidence is required for %s", record.HeadCommit, head)
			}
		}
		need("verification-record/1", keyErr)
		err = executorPremerge(ctx, premerge, id.Node, n, c)
		need("pre_merge", err)
	}
	review, _, err := s.agentReview(ctx, q, id.Plan, id.Node, e, head, tree)
	if err == nil && review.CriteriaDigest != n.CriteriaSetDigest {
		err = errors.New("review/unit-check criteria differ from the current plan")
	}
	need("review_or_unit_check", err)
	return gates, nil
}
