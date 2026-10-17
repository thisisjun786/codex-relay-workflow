package ops

import (
	"context"
	"errors"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// ExecutorResultCheck is shared by controller commands and future collection
// operations. Evidence bytes are explicit controller inputs, not artifact URLs.
type ExecutorResultCheck struct {
	Execution, Actor, Spool, ResultPath, CollectionRoot string
	Verification, Premerge                              []byte
}
type ExecutorResultChecker interface {
	ExecutorCheckContext(context.Context, string, string) (dagsched.ExecutorCheckContext, error)
	CheckExecutorGates(context.Context, executor.Identity, string, *executor.CollectedCandidate, []byte, []byte) ([]executor.RequiredGate, error)
}

func CheckExecutorResult(ctx context.Context, s ExecutorResultChecker, in ExecutorResultCheck) (executor.ResultCheckReport, error) {
	bound, err := s.ExecutorCheckContext(ctx, in.Execution, in.Actor)
	if err != nil {
		return executor.ResultCheckReport{}, err
	}
	b := bound.Admission.Task.Executor
	report := executor.CheckExecutorResult(ctx, executor.ResultCheckInput{Admission: bound.Admission, Peer: executor.Peer{HostID: b.HostID, Incarnation: b.Incarnation}, Spool: in.Spool, ResultPath: in.ResultPath, CollectionRoot: in.CollectionRoot, Repository: bound.Repository, ExpectedTools: bound.ExpectedTools})
	if !report.EnvelopeValid || !report.CandidateValid {
		return report, nil
	}
	// Collection can take time: re-read assignment, host and plan before reporting
	// gate judgments. A different revision never inherits the fetched proof.
	current, err := s.ExecutorCheckContext(ctx, in.Execution, in.Actor)
	if err != nil {
		return report, err
	}
	if current.Admission.TaskDigest != bound.Admission.TaskDigest {
		return report, errors.New("assignment changed during collection")
	}
	gates, err := s.CheckExecutorGates(ctx, bound.Admission.Task.Identity, in.Actor, report.Candidate, in.Verification, in.Premerge)
	if err != nil {
		return report, err
	}
	kept := report.RequiredGates[:0]
	for _, g := range report.RequiredGates {
		switch g.Gate {
		case "verification-record/1", "pre_merge", "review_or_unit_check":
		default:
			kept = append(kept, g)
		}
	}
	report.RequiredGates = append(kept, gates...)
	return report, nil
}
