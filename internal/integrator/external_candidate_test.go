package integrator

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

type externalAuthority struct {
	candidate                    executor.CollectedExecution
	acceptErr                    error
	reads, accepts, integrations int
	expire                       bool
	accepted                     dagsched.AcceptInput
}

func (a *externalAuthority) CollectedExecutor(context.Context, string, string) (executor.CollectedExecution, error) {
	a.reads++
	if a.expire && a.reads > 1 {
		return executor.CollectedExecution{}, errors.New("owner expired")
	}
	return a.candidate, nil
}
func (a *externalAuthority) Accept(_ context.Context, _, _, _ string, in dagsched.AcceptInput) (dagsched.AcceptResult, error) {
	a.accepts++
	a.accepted = in
	return dagsched.AcceptResult{}, a.acceptErr
}
func (a *externalAuthority) IntegrateBatch(context.Context, dagsched.IntegrationBatchInput, dagsched.IntegrationBatchDeps) (dagsched.IntegrationBatchResult, error) {
	a.integrations++
	return dagsched.IntegrationBatchResult{}, nil
}

func TestExternalCandidateInputUsesAcceptanceAndIntegrationGates(t *testing.T) {
	for _, mode := range []string{"pass", "acceptance refused", "late owner", "target changed"} {
		t.Run(mode, func(t *testing.T) {
			w := newWorld(t)
			base := w.remoteDev()
			head := w.head("external", map[string]string{"file": "external"}, false)
			w.git(w.other, "remote", "set-url", "origin", "https://github.com/owner/repo.git")
			w.git(w.checkout, "remote", "set-url", "origin", "https://github.com/owner/repo.git")
			task := executor.Task{Schema: executor.TaskSchema, Identity: executor.Identity{TaskID: "task", ExecutionID: "execution", Attempt: 1, AssignmentGeneration: 1, Plan: "plan", Node: "node", Revision: "1", CriteriaDigest: strings.Repeat("a", 64)}, Repository: executor.Repository{Forge: "github", ID: "owner/repo", BaseCommit: base, TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/external"}, Regions: []executor.Region{{Repository: "owner/repo", Kind: "tree", Path: "."}}, WorkKind: "implementation", Executor: executor.Binding{HostID: "host", Incarnation: "inc", Kind: executor.Devin, CapabilityRevision: "cap", LaunchProfileDigest: strings.Repeat("b", 64), ToolchainProfileDigest: strings.Repeat("c", 64)}, Requested: executor.Requested{Model: "fixture", Effort: "high"}, Input: executor.Input{Kind: "prompt", Prompt: "work", BriefDigest: executor.Digest([]byte("work"))}, Checks: []executor.Check{{ID: "test", Args: []string{"true"}, Resources: executor.Resources{CPU: 1}}}}
			admission, err := executor.Freeze(task)
			if err != nil {
				t.Fatal(err)
			}
			c := executor.CollectedExecution{Admission: admission, ExecutionID: "execution", Candidate: executor.CollectedCandidate{Repository: "owner/repo", Checkout: w.other, Base: base, Head: head, Tree: w.git(w.other, "rev-parse", "HEAD^{tree}")}, Receipt: executor.Acknowledgement{State: "received", ExecutionID: "execution", TaskDigest: admission.TaskDigest, ResultDigest: executor.Digest([]byte("result"))}}
			authority := &externalAuthority{candidate: c}
			in := ExternalCandidateInput{Execution: "execution", Actor: "parent", Verification: "controller receipt", Premerge: []byte("controller premerge"), Integration: dagsched.IntegrationBatchInput{Plan: "plan", Actor: "parent", Checkout: w.checkout, IntegrationRef: "dev", BaseRef: "dev", Nodes: []string{"node"}}}
			switch mode {
			case "acceptance refused":
				authority.acceptErr = errors.New("no genuine review")
			case "late owner":
				authority.expire = true
			case "target changed":
				in.Integration.IntegrationRef = "main"
			}
			before := w.remoteDev()
			_, err = IntegrateExternalCandidate(context.Background(), authority, in, dagsched.IntegrationBatchDeps{})
			if (err == nil) != (mode == "pass") {
				t.Fatalf("%s: %v", mode, err)
			}
			if mode == "pass" {
				if authority.accepts != 1 || authority.integrations != 1 || authority.accepted.Commit.Head != head || authority.accepted.Commit.Checkout != w.other {
					t.Fatalf("gates: %+v", authority)
				}
			} else if authority.integrations != 0 {
				t.Fatal("refused candidate reached integration")
			}
			if w.remoteDev() != before {
				t.Fatal("external candidate input pushed target")
			}
		})
	}
}
