package dagsched

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func executorFixtureCollect(t *testing.T, repo, base, head, branch string) *executor.CollectedCandidate {
	t.Helper()
	task := executor.Task{Schema: executor.TaskSchema,
		Identity:   executor.Identity{TaskID: "t", ExecutionID: "e", Attempt: 1, AssignmentGeneration: 1, Plan: "g", Node: "candidate", Revision: "1", CriteriaDigest: strings.Repeat("a", 64)},
		Repository: executor.Repository{Forge: "github", ID: "owner/repo", BaseCommit: base, TargetRef: "refs/heads/dev", TaskBranch: branch},
		Regions:    []executor.Region{{Path: ".", Kind: "tree"}}, WorkKind: "implementation",
		Executor:  executor.Binding{HostID: "h", Incarnation: "i", Kind: executor.Devin, CapabilityRevision: "1", LaunchProfileDigest: strings.Repeat("b", 64), ToolchainProfileDigest: strings.Repeat("c", 64)},
		Requested: executor.Requested{Model: "model", Effort: "high"}, Input: executor.Input{Kind: "prompt", Prompt: "work", BriefDigest: executor.Digest([]byte("work"))},
		Checks: []executor.Check{{ID: "check", Args: []string{"true"}, Resources: executor.Resources{CPU: 1}}}}
	a, err := executor.Freeze(task)
	if err != nil {
		t.Fatal(err)
	}
	r := &gitRepo{t: t, path: repo}
	want := executor.Candidate{Branch: branch, Head: head, Tree: r.git("rev-parse", head+"^{tree}")}
	c, err := executor.CollectCandidate(context.Background(), a, want, executor.RegisteredRepository{Forge: "github", ID: "owner/repo", Source: repo}, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestExecutorCheckPremergeDoesNotRunGlobalMergeDriver(t *testing.T) {
	r := newGitRepo(t)
	r.commit(".gitattributes", "base.txt merge=reviewprobe\n")
	_ = r.git("rev-parse", "HEAD")
	r.git("checkout", "-b", "candidate")
	evaluated := r.commit("base.txt", "candidate\n")
	r.git("checkout", "dev")
	target := r.commit("base.txt", "target\n")
	r.git("checkout", "candidate")
	// Construct a merge with an explicit tree without asking Git to merge content.
	tree := r.git("rev-parse", evaluated+"^{tree}")
	head := r.git("commit-tree", tree, "-p", evaluated, "-p", target, "-m", "merged")
	r.git("reset", "--hard", head)
	c := executorFixtureCollect(t, r.path, target, head, "refs/heads/candidate")
	marker := filepath.Join(t.TempDir(), "driver-executed")
	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[merge \"reviewprobe\"]\n\tdriver = touch "+marker+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	// This is the same call chain CheckExecutorGates uses for an older premerge head.
	k := newAgentKit(t, agentSpec{name: "candidate", files: map[string]string{"candidate.txt": "work\n"}})
	id := executor.Identity{Plan: "g", Node: "candidate", ExecutionID: k.execution("candidate"), CriteriaDigest: releaseCriteriaDigest()}
	gates, err := k.sched.CheckExecutorGates(context.Background(), id, "parent", c, nil, premergeAt(k.sched, context.Background(), "g", "candidate", evaluated))
	if err != nil || !gateNamed(gates, "pre_merge") {
		t.Fatalf("hand-resolved merge lost its required gate: %+v %v", gates, err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("read-only gate executed the configured merge driver from candidate attributes")
	}
}

func TestExecutorPremergeIsolatedReuseMatchesExistingRules(t *testing.T) {
	r := newGitRepo(t)
	base := r.git("rev-parse", "HEAD")
	r.git("checkout", "-b", "candidate")
	evaluated := r.commit("candidate.txt", "candidate\n")
	plain := r.commit("later.txt", "later\n")
	if after, err := executorPremergeHeadRelation(context.Background(), r.path, base, evaluated, plain); err != nil || !after {
		t.Fatalf("plain commit: after=%v %v", after, err)
	}
	r.git("checkout", "dev")
	target := r.commit("target.txt", "target\n")
	r.git("checkout", "candidate")
	r.git("reset", "--hard", evaluated)
	r.git("merge", "--no-edit", target)
	head := r.git("rev-parse", "HEAD")
	if after, err := executorPremergeHeadRelation(context.Background(), r.path, target, evaluated, head); err != nil || after {
		t.Fatalf("automatic dev merge: after=%v %v", after, err)
	}
	if _, err := executorPremergeHeadRelation(context.Background(), r.path, base, evaluated, head); err == nil {
		t.Fatal("merge outside frozen base preserved evaluation")
	}
}
