package engine

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

func reviewCandidate(t *testing.T) executor.CollectedExecution {
	t.Helper()
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=fixture", "GIT_COMMITTER_NAME=fixture", "GIT_AUTHOR_EMAIL=fixture@example.invalid", "GIT_COMMITTER_EMAIL=fixture@example.invalid")
		raw, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git: %s %v", raw, err)
		}
		return strings.TrimSpace(string(raw))
	}
	git("init", "-b", "dev")
	git("commit", "--allow-empty", "-m", "base")
	base := git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "file.txt"), []byte("candidate"), 0600); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-m", "candidate")
	git("remote", "add", "origin", "https://github.com/owner/repo.git")
	task := enTask("n1", "check the controller criteria", Pair{Model: "synthetic", Effort: "high"})
	task.Identity.CriteriaDigest = delivery.SetDigest(externalReviewCriteria())
	task.Repository.ID = "owner/repo"
	task.Repository.BaseCommit = base
	task.Regions = []executor.Region{{Repository: "owner/repo", Kind: "tree", Path: "."}}
	a, err := executor.Freeze(task)
	if err != nil {
		t.Fatal(err)
	}
	return executor.CollectedExecution{Admission: a, ExecutionID: task.Identity.ExecutionID, Candidate: executor.CollectedCandidate{Repository: task.Repository.ID, Checkout: dir, Head: git("rev-parse", "HEAD"), Tree: git("rev-parse", "HEAD^{tree}"), Base: base}, Receipt: executor.Acknowledgement{State: "received", ExecutionID: task.Identity.ExecutionID, TaskDigest: a.TaskDigest, ResultDigest: executor.Digest([]byte("result"))}}
}
func TestExternalCandidateReviewIsActualEngineCall(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "current", true: "owner changed during review"}[stale], func(t *testing.T) {
			h := newHarness(t)
			bound := reviewCandidate(t)
			current := true
			h.cfg.ExecutorCandidate = func(context.Context, SnapshotRequest) (*ExecutorCandidateReview, error) {
				if !current {
					return nil, errors.New("owner epoch changed")
				}
				return &ExecutorCandidateReview{CollectedExecution: bound, Criteria: externalReviewCriteria()}, nil
			}
			h.fake.StartHook = func(spec ThreadSpec) (ThreadInfo, error) {
				if spec.Cwd != bound.Candidate.Checkout || spec.Head != bound.Candidate.Head {
					t.Errorf("review did not use collection: %+v", spec)
				}
				return ThreadInfo{Pair: spec.Pair, Cwd: spec.Cwd}, nil
			}
			h.fake.Handler = func(req FakeRequest) FakeReply {
				if !strings.Contains(req.Prompt, "current acceptance title") {
					t.Error("review received only the criteria digest, not current criteria text")
				}
				if !strings.Contains(req.Prompt, bound.Admission.Task.Identity.CriteriaDigest) {
					t.Error("review did not receive current criteria")
				}
				if stale {
					current = false
				}
				return FakeReply{}
			}
			res := h.run(`return await agent.call('review it', {agentType:'reviewer',node:'n1',schema:VERDICT})`)
			if res.Status != RunCompleted {
				t.Fatalf("run: %+v", res)
			}
			n := 0
			for _, ev := range h.journal.Events() {
				if ev.Kind == KindCall && ev.Event == EvCompleted {
					n++
					if ev.Data["execution_id"] != bound.ExecutionID || ev.Data["head"] != bound.Candidate.Head || ev.Data["criteria_digest"] != bound.Admission.Task.Identity.CriteriaDigest {
						t.Fatalf("unbound genuine call: %+v", ev)
					}
				}
			}
			if stale && n != 0 || !stale && n != 1 {
				t.Fatalf("completed reviews %d (stale=%t)", n, stale)
			}
			if len(h.fake.Requests()) != 2 {
				t.Fatalf("expected actual thread and turn: %+v", h.fake.Requests())
			}
		})
	}
}

func externalReviewCriteria() []delivery.Criterion {
	return []delivery.Criterion{{ID: "criterion", Title: "current acceptance title", Required: true}}
}
