package dagsched

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"os"
	"path/filepath"
	"testing"
)

func TestExternalCandidateGateReadsActualCheckoutHead(t *testing.T) {
	k := newAgentKit(t, agentSpec{name: "candidate", files: map[string]string{"candidate.txt": "work\n"}})
	in := k.input("candidate")
	raw, err := readVerificationRecord(in.Commit.Record)
	if err != nil {
		t.Fatal(err)
	}
	c := &executor.CollectedCandidate{Checkout: k.worktrees["candidate"], Head: k.heads["candidate"], Tree: k.treeOf(k.heads["candidate"]), Base: k.base}
	id := executor.Identity{Plan: "g", Node: "candidate", ExecutionID: k.execution("candidate"), CriteriaDigest: releaseCriteriaDigest()}
	k.review("candidate", reviewerModel, "verified", id.CriteriaDigest)
	k.repo.git("-C", c.Checkout, "checkout", "--detach", k.base)
	gates, err := k.sched.CheckExecutorGates(context.Background(), id, "parent", c, raw, in.Premerge)
	if err == nil && len(gates) == 0 {
		t.Fatal("a stale candidate object passed while actual controller HEAD moved")
	}
}

func externalGateKit(t *testing.T) (*agentKit, executor.CollectedExecution) {
	t.Helper()
	ctx := context.Background()
	k := newAgentKit(t, agentSpec{name: "candidate", unreleased: true, files: map[string]string{"node-candidate.txt": "work\n"}})
	k.tips.sha = k.base
	h := enrolled(t, k.releaseKit, "external-host", 1)
	a, err := k.sched.ReleaseExecutor(ctx, "g", "candidate", "parent", k.agentRequest(true), AgentReleaseOptions{RunID: "run-1", Worktree: "/worker/path-not-on-controller"}, placement(h))
	if err != nil {
		t.Fatal(err)
	}
	task := a.Admission.Task
	k.repo.git("update-ref", task.Repository.TaskBranch, k.heads["candidate"])
	c, err := executor.CollectCandidate(ctx, a.Admission, executor.Candidate{Branch: task.Repository.TaskBranch, Head: k.heads["candidate"], Tree: k.treeOf(k.heads["candidate"])}, executor.RegisteredRepository{Forge: "github", ID: "owner/repo", Source: k.repo.path}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	bound := executor.CollectedExecution{Admission: a.Admission, Candidate: *c, ExecutionID: task.Identity.ExecutionID, Receipt: executor.Acknowledgement{State: "received", ExecutionID: task.Identity.ExecutionID, TaskDigest: a.Admission.TaskDigest, ResultDigest: executor.Digest([]byte("authenticated fixture"))}}
	if err = k.sched.RecordExecutorCollection(ctx, bound.ExecutionID, "parent", bound); err != nil {
		t.Fatal(err)
	}
	return k, bound
}
func TestExternalCandidateAcceptanceUsesCollectionAndRealReview(t *testing.T) {
	k, c := externalGateKit(t)
	ctx := context.Background()
	in := k.input("candidate")
	in.Commit.Checkout = c.Candidate.Checkout
	if _, err := k.sched.Accept(ctx, "g", "candidate", "parent", in); err == nil {
		t.Fatal("external narrative substituted for genuine review")
	}
	k.review("candidate", reviewerModel, "verified", releaseCriteriaDigest())
	if _, err := k.sched.Accept(ctx, "g", "candidate", "parent", in); err != nil {
		t.Fatal(err)
	}
	var workerPath string
	if err := k.s.DB.QueryRow(`SELECT worktree FROM dag_agent_executions WHERE execution_id=?`, c.ExecutionID).Scan(&workerPath); err != nil {
		t.Fatal(err)
	}
	if workerPath != "/worker/path-not-on-controller" {
		t.Fatal("rewrote existing execution worktree semantics")
	}
}
func TestExternalCandidateAcceptanceRejectsOtherTreeBaseAndLateOwner(t *testing.T) {
	for _, which := range []string{"tree", "base", "owner", "checkout"} {
		t.Run(which, func(t *testing.T) {
			k, c := externalGateKit(t)
			ctx := context.Background()
			in := k.input("candidate")
			in.Commit.Checkout = c.Candidate.Checkout
			k.review("candidate", reviewerModel, "verified", releaseCriteriaDigest())
			switch which {
			case "tree":
				in.Commit.Record = writeSealedRecord(t, k.repo.path, k.base, k.base, k.treeOf(k.base), "pass")
			case "base":
				in.Commit.Base = c.Candidate.Head
			case "checkout":
				in.Commit.Checkout = k.worktrees["candidate"]
			case "owner":
				k.sched.testBeforeAcceptTx = func() { k.replaceParent("successor") }
			}
			if _, err := k.sched.Accept(ctx, "g", "candidate", "parent", in); err == nil {
				t.Fatalf("%s accepted", which)
			}
			if k.agentAcceptanceRows() != 0 {
				t.Fatal("refusal wrote acceptance")
			}
		})
	}
}
func TestExternalCandidateConfiguredCommandSurvivesCollection(t *testing.T) {
	k, c := externalGateKit(t)
	config := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(config, []byte(`{"repositories":{"owner/repo":{"verify_command":"printf registered-verifier"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CRW_CONFIG", config)
	record, err := VerifyRepositoryCommand(context.Background(), c.Candidate.Checkout, c.Candidate.Head, c.Candidate.Base, "printf registered-verifier")
	if err != nil || record.Result != "pass" {
		t.Fatalf("verify: %+v %v", record, err)
	}
	raw, err := SealVerificationRecord(record)
	if err != nil {
		t.Fatal(err)
	}
	recordPath := filepath.Join(t.TempDir(), "verification.json")
	if err = os.WriteFile(recordPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	in := k.input("candidate")
	in.Commit.Checkout = c.Candidate.Checkout
	in.Commit.Record = "@" + recordPath
	k.review("candidate", reviewerModel, "verified", releaseCriteriaDigest())
	if _, err = k.sched.Accept(context.Background(), "g", "candidate", "parent", in); err != nil {
		t.Fatal(err)
	}
}
