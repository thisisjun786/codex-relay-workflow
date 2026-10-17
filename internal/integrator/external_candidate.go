package integrator

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// ExternalCandidateInput carries controller evidence, never worker commands or
// a worker checkout path. Integration retains the existing batch/CAS verifier.
type ExternalCandidateInput struct {
	Execution, Actor string
	Verification     string
	Premerge         []byte
	Rule             dagsched.VerifierRule
	Integration      dagsched.IntegrationBatchInput
}

type ExternalCandidateAuthority interface {
	CollectedExecutor(context.Context, string, string) (executor.CollectedExecution, error)
	Accept(context.Context, string, string, string, dagsched.AcceptInput) (dagsched.AcceptResult, error)
	IntegrateBatch(context.Context, dagsched.IntegrationBatchInput, dagsched.IntegrationBatchDeps) (dagsched.IntegrationBatchResult, error)
}

// IntegrateExternalCandidate imports the immutable collected object, accepts it
// through dag-accept and asks the existing integrator to verify the merged tree.
// This entry point does not push: a worker result never grants push authority.
func IntegrateExternalCandidate(ctx context.Context, authority ExternalCandidateAuthority, in ExternalCandidateInput, deps dagsched.IntegrationBatchDeps) (dagsched.IntegrationBatchResult, error) {
	var zero dagsched.IntegrationBatchResult
	if authority == nil {
		return zero, errors.New("external integration needs controller authority")
	}
	c, err := authority.CollectedExecutor(ctx, in.Execution, in.Actor)
	if err != nil {
		return zero, err
	}
	if err = c.Inspect(ctx); err != nil {
		return zero, err
	}
	task := c.Admission.Task
	canonical := func(ref string) string { return "refs/heads/" + strings.TrimPrefix(ref, "refs/heads/") }
	if in.Integration.Plan != task.Identity.Plan || in.Integration.Actor != in.Actor || canonical(in.Integration.IntegrationRef) != task.Repository.TargetRef || (in.Integration.BaseRef != "" && canonical(in.Integration.BaseRef) != task.Repository.TargetRef) {
		return zero, errors.New("external integration target differs from current assignment")
	}
	if len(in.Integration.Nodes) != 1 || in.Integration.Nodes[0] != task.Identity.Node {
		return zero, errors.New("external input names only its own node")
	}
	git := func(args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", append([]string{"-c", "core.hooksPath=/dev/null", "-C", in.Integration.Checkout}, args...)...)
		for _, v := range os.Environ() {
			if !strings.HasPrefix(v, "GIT_") {
				cmd.Env = append(cmd.Env, v)
			}
		}
		cmd.Env = append(cmd.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_NO_REPLACE_OBJECTS=1", "GIT_TERMINAL_PROMPT=0")
		raw, e := cmd.Output()
		if e != nil {
			return "", fmt.Errorf("controller candidate import: git %s: %w", args[0], e)
		}
		return strings.TrimSpace(string(raw)), nil
	}
	origin, err := git("remote", "get-url", "origin")
	if err != nil {
		return zero, err
	}
	if origin != "https://github.com/"+task.Repository.ID+".git" && origin != "git@github.com:"+task.Repository.ID+".git" {
		return zero, errors.New("integration checkout differs from registered repository")
	}
	if _, err = git("-c", "protocol.file.allow=always", "fetch", "--no-tags", "--no-recurse-submodules", "--", c.Candidate.Checkout, c.Candidate.Head); err != nil {
		return zero, err
	}
	if _, err = authority.Accept(ctx, task.Identity.Plan, task.Identity.Node, in.Actor, dagsched.AcceptInput{RuleVersion: in.Rule, Premerge: in.Premerge, Commit: &dagsched.CommitRef{Head: c.Candidate.Head, Base: c.Candidate.Base, Checkout: c.Candidate.Checkout, Record: in.Verification}}); err != nil {
		return zero, err
	}
	// Acceptance and integration have separate authority checks. A handover or
	// plan change between them cannot use this earlier collection as permission.
	current, err := authority.CollectedExecutor(ctx, in.Execution, in.Actor)
	if err != nil {
		return zero, err
	}
	if current.ExecutionID != c.ExecutionID || current.Admission.TaskDigest != c.Admission.TaskDigest || current.Candidate.Head != c.Candidate.Head {
		return zero, errors.New("candidate changed after acceptance")
	}
	return authority.IntegrateBatch(ctx, in.Integration, deps)
}
