package agent_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

func TestExecutorAdmissionRecordsNativeProvenance(t *testing.T) {
	e := newEnv(t)
	prompt := "build it"
	task := executor.Task{
		Schema:   executor.TaskSchema,
		Identity: executor.Identity{TaskID: "task-1", ExecutionID: "exec-1", Attempt: 1, AssignmentGeneration: 1, Plan: "plan-1", Node: "n1", Revision: "r0", CriteriaDigest: strings.Repeat("a", 64)},
		Repository: executor.Repository{Forge: "github", ID: "repo-1", BaseCommit: strings.Repeat("b", 40),
			TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/n1"},
		Regions:  []executor.Region{{Path: "x/f.go"}},
		WorkKind: "implementation",
		Executor: executor.Binding{HostID: "host-1", Incarnation: "inc-1", Kind: executor.Codex, CapabilityRevision: "cap-1",
			LaunchProfileDigest: strings.Repeat("c", 64), ToolchainProfileDigest: strings.Repeat("d", 64)},
		Requested: executor.Requested{Model: "claude-fake-haiku", Effort: "xhigh"},
		Input:     executor.Input{Kind: "prompt", Prompt: prompt, BriefDigest: executor.Digest([]byte(prompt))},
		Checks:    []executor.Check{{ID: "check-1", Args: []string{"true"}, Resources: executor.Resources{CPU: 1}}},
	}
	called := 0
	e.cfg.executorAdmission = func(ctx context.Context, req engine.SnapshotRequest) (executor.Admission, error) {
		called++
		if req.Node != "n1" || req.Role != "executor" {
			t.Errorf("the resolver saw %+v", req)
		}
		return executor.Freeze(task)
	}
	res := e.run(`const S = ` + verdictSchema + `
const b = await agent.call('build it', { agentType: 'executor', node: 'n1', schema: S, label: 'build' })
const r = await agent.call('review it', { agentType: 'ocx-gpt-6-1-sol', node: 'n1', head: ` + q(e.base) + `, schema: S, label: 'review' })
return [b.status, r.status]`)
	if got := value[[]string](t, res); got[0] != "completed" || got[1] != "completed" {
		t.Fatalf("%v", got)
	}
	if called != 1 {
		t.Fatalf("the resolver ran %d times", called)
	}
	var native *engine.Event
	for i, ev := range e.journal.Events() {
		if ev.Event == "executor_native" {
			native = &e.journal.Events()[i]
		}
	}
	if native == nil {
		t.Fatal("no executor_native row")
	}
	by := calls(res)
	praw, _ := json.Marshal(native.Data["provenance"])
	var prov map[string]any
	if err := json.Unmarshal(praw, &prov); err != nil || prov == nil {
		t.Fatalf("%+v", native.Data)
	}
	if sess, _ := prov["native_session"].(map[string]any); sess["value"] != by["build"].Meta.ThreadID {
		t.Errorf("session %v vs %s", sess, by["build"].Meta.ThreadID)
	}
	turns, _ := prov["native_turns"].([]any)
	if len(turns) != len(by["build"].Meta.TurnIDs) {
		t.Fatalf("turns %v vs %v", turns, by["build"].Meta.TurnIDs)
	}
	for i, tn := range turns {
		if tn != by["build"].Meta.TurnIDs[i] {
			t.Errorf("turn %d: %v vs %s", i, tn, by["build"].Meta.TurnIDs[i])
		}
	}
	if starts := e.requests("thread/start"); len(starts) != 2 {
		t.Errorf("%d thread/start requests; provenance added a call", len(starts))
	}
}
