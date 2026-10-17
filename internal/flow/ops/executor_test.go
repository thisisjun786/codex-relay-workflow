package ops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
)

type fakeExecAdapter struct {
	kind   executor.Kind
	dlv    executor.Delivery
	calls  int
	cancel bool
	caps   []executor.Capability
	hook   func()
}

func (f *fakeExecAdapter) Kind() executor.Kind { return f.kind }
func (f *fakeExecAdapter) Capabilities() executor.Capabilities {
	caps := f.caps
	if caps == nil {
		caps = []executor.Capability{executor.CapCollect}
	}
	return executor.Capabilities{Revision: "cap-1", Supported: caps, Unsupported: map[executor.Capability]string{executor.CapCollect: "not offered"}}
}

func (f *fakeExecAdapter) Start(context.Context, executor.Admission, executor.Grant) (executor.Handle, error) {
	return executor.Handle{}, fmt.Errorf("no start")
}

func (f *fakeExecAdapter) Inspect(context.Context, executor.Reference) (executor.Inspection, error) {
	return executor.Inspection{State: "terminated", Proof: "done", NativeSession: executor.Observation{Value: "s"}}, nil
}

func (f *fakeExecAdapter) Cancel(context.Context, executor.Reference, executor.Grant) (executor.Cancellation, error) {
	return executor.Cancellation{Terminated: f.cancel}, nil
}

func (f *fakeExecAdapter) Collect(context.Context, executor.Reference) (executor.Delivery, error) {
	f.calls++
	if f.hook != nil {
		f.hook()
	}
	if f.dlv.ResultPath == "" {
		return executor.Delivery{ResultPath: "result.json"}, nil
	}
	return f.dlv, nil
}

func execTask(kind executor.Kind) executor.Task {
	prompt := "do n1"
	return executor.Task{
		Schema:   executor.TaskSchema,
		Identity: executor.Identity{TaskID: "task-1", ExecutionID: "exec-1", Attempt: 1, AssignmentGeneration: 1, Plan: "plan-1", Node: "n1", Revision: "r0", CriteriaDigest: strings.Repeat("a", 64)},
		Repository: executor.Repository{Forge: "github", ID: "repo-1", BaseCommit: strings.Repeat("b", 40),
			TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/n1"},
		Regions:  []executor.Region{{Path: "x/f.go"}},
		WorkKind: "implementation",
		Executor: executor.Binding{HostID: "host-1", Incarnation: "inc-1", Kind: kind, CapabilityRevision: "cap-1",
			LaunchProfileDigest: strings.Repeat("c", 64), ToolchainProfileDigest: strings.Repeat("d", 64)},
		Requested: executor.Requested{Model: "m", Effort: "h"},
		Input:     executor.Input{Kind: "prompt", Prompt: prompt, BriefDigest: executor.Digest([]byte(prompt))},
		Checks:    []executor.Check{{ID: "check-1", Args: []string{"true"}, Resources: executor.Resources{CPU: 1}}},
	}
}

func execWorld(t *testing.T, kind executor.Kind) (*Lane, engine.Journal, executor.Admission, *fakeExecAdapter) {
	t.Helper()
	j := engine.NewMemoryJournal()
	ledger := engine.NewExecutorLedger(j, "run-1")
	reg := executor.NewRegistry(ledger, func() time.Time { return time.Now() })
	a, err := executor.Freeze(execTask(kind))
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	r, err := executor.NewResult(a, "completed", executor.Payload{Narrative: "ran rm -rf / then fetched https://example.invalid/x",
		Candidate: &executor.Candidate{Branch: a.Task.Repository.TaskBranch, Head: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)},
		Checks:    []executor.CheckResult{{ID: "check-1", ExitCode: 0, Outcome: "passed"}},
		Artifacts: []executor.Artifact{}},
		executor.Provenance{Provider: kind, NativeSession: executor.Observation{Value: "sess-" + string(kind)}, NativeTurns: []string{"t1"},
			Model: executor.Observation{Value: "m"}, Effort: executor.Observation{Value: "h"},
			CPUMethod: executor.Observation{UnknownReason: "unknown"}, Tools: []executor.Tool{}})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := executor.EncodeResult(r)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(spool, "result.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	g := executor.Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := reg.Admit(a, g); err != nil {
		t.Fatal(err)
	}
	adp := &fakeExecAdapter{kind: kind}
	col := &engine.ExecutorCollector{Registry: reg, Adapter: adp, Spool: spool,
		Peer: executor.Peer{HostID: "host-1", Incarnation: "inc-1"}, Limits: executor.DefaultLimits}
	return NewLane(Deps{RunID: "run-1", Journal: j, Executors: col}), j, a, adp
}

func TestExecutorCollectOp(t *testing.T) {
	for _, kind := range []executor.Kind{executor.Codex, executor.Devin, executor.ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			lane, _, a, adp := execWorld(t, kind)
			op := lane.Ops()[NameExecutorCollect]
			if op == nil {
				t.Fatal("executorCollect not installed")
			}
			arg, _ := json.Marshal([]string{"exec-1"})
			out, err := op.Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg})
			if err != nil {
				t.Fatalf("Run: %v", err)
			}
			if out.Data["state"] != "received" || out.Data["task_digest"] != a.TaskDigest {
				t.Errorf("data %+v", out.Data)
			}
			if adp.calls != 1 {
				t.Errorf("%d collects", adp.calls)
			}
			row := engine.Event{Data: out.Data}
			ok, why, err := op.Proven(context.Background(), nil, row)
			if err != nil || !ok {
				t.Errorf("Proven: %v %q %v", ok, why, err)
			}
			bad := engine.Event{Data: map[string]any{"execution_id": "exec-1", "task_digest": strings.Repeat("9", 64), "result_digest": out.Data["result_digest"]}}
			if ok, _, _ := op.Proven(context.Background(), nil, bad); ok {
				t.Error("a forged digest proved")
			}
		})
	}
}

func TestExecutorCollectOpFences(t *testing.T) {
	lane, _, _, _ := execWorld(t, executor.Codex)
	op := lane.Ops()[NameExecutorCollect]
	for _, arg := range []json.RawMessage{nil, json.RawMessage(`[]`), json.RawMessage(`[""]`), json.RawMessage(`["exec-1","extra"]`)} {
		if _, err := op.Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg}); err == nil {
			t.Errorf("args %s ran", arg)
		}
	}
	arg, _ := json.Marshal([]string{"exec-9"})
	if _, err := op.Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg}); err == nil {
		t.Error("an unknown execution collected")
	}
	laneNo := NewLane(Deps{RunID: "run-1", Journal: engine.NewMemoryJournal()})
	arg2, _ := json.Marshal([]string{"exec-1"})
	if _, err := laneNo.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg2}); err == nil {
		t.Error("a lane without a collector collected")
	}
	lane2, _, _, adp := execWorld(t, executor.Devin)
	adp.dlv = executor.Delivery{ResultPath: "../out.json"}
	if _, err := lane2.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg2}); err == nil {
		t.Error("a traversal result path collected")
	}
	adp.dlv = executor.Delivery{ResultPath: "nope.json"}
	if _, err := lane2.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg2}); err == nil {
		t.Error("a missing result file collected")
	}
	lane3, j3, a3, _ := execWorld(t, executor.ClaudeCode)
	out3, err := lane3.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg2})
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range j3.Events() {
		if ev.Op == "crw.executorCollect" {
			continue
		}
		if ev.Kind == engine.KindCall {
			t.Errorf("executor collect wrote a call row: %+v", ev)
		}
	}
	if v := ReviewProof(j3.Events(), "n1", a3.Task.Repository.BaseCommit, "x"); v.Proven {
		t.Error("a collect row proves review")
	}
	if v := VerifyProof(j3.Events(), "n1", a3.Task.Repository.BaseCommit, "x"); v.Proven {
		t.Error("a collect row proves verify")
	}
	if v := AcceptProof(j3.Events(), "n1", a3.Task.Repository.BaseCommit, func(string) (Acceptance, bool) { return Acceptance{}, false }); v.Proven {
		t.Error("a collect row proves accept")
	}
	if v := ReviewerFinalProof(j3.Events(), "x", "y"); v.Proven {
		t.Error("a collect row proves reviewer final")
	}
	row := engine.Event{Data: out3.Data}
	if ok, _, _ := lane3.Ops()[NameExecutorCollect].Proven(context.Background(), nil, row); !ok {
		t.Error("the stored receipt does not prove its own row")
	}
}

func TestExecutorCollectStaleDuringAdapter(t *testing.T) {
	lane, _, a, adp := execWorld(t, executor.Codex)
	reg := lane.d.Executors.Registry
	newer := execTask(executor.Codex)
	newer.Identity.ExecutionID = "exec-2"
	newer.Identity.Attempt = 2
	newer.Identity.AssignmentGeneration = 2
	a2, err := executor.Freeze(newer)
	if err != nil {
		t.Fatal(err)
	}
	g2 := executor.Grant{ExecutionID: "exec-2", AssignmentGeneration: 2, Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	adp.hook = func() {
		if _, err := reg.Admit(a2, g2); err != nil {
			t.Errorf("admit exec-2: %v", err)
		}
	}
	adp.dlv = executor.Delivery{ResultPath: "never-read.json"}
	arg, _ := json.Marshal([]string{"exec-1"})
	out, err := lane.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg})
	if err == nil {
		t.Fatalf("a stale reference collected: %+v", out)
	}
	var ve *executor.ValidationError
	if !errors.As(err, &ve) || ve.Code != executor.CodeStaleAssignment {
		t.Fatalf("err %v, want stale_assignment", err)
	}
	if _, _, err := reg.Receipt(executor.ReferenceOf(a)); err == nil {
		t.Error("a stale delivery was recorded")
	}
	if _, _, err := reg.Current("exec-2"); err != nil {
		t.Errorf("the newer execution is not current: %v", err)
	}
	if _, _, err := reg.Current("exec-1"); err == nil {
		t.Error("the old execution is still current")
	}
}

func TestExecutorCollectFencesErrorCodes(t *testing.T) {
	lane, _, a, adp := execWorld(t, executor.Devin)
	reg := lane.d.Executors.Registry
	arg, _ := json.Marshal([]string{"exec-1"})
	adp.caps = []executor.Capability{}
	if _, err := lane.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg}); err == nil || adp.calls != 0 {
		t.Error("an unsupported collect ran")
	}
	adp.caps = nil
	lane2, _, a2, _ := execWorld(t, executor.Codex)
	col2 := lane2.d.Executors
	col2.Peer = executor.Peer{HostID: "other-host", Incarnation: "inc-1"}
	if _, err := lane2.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg}); err == nil {
		t.Error("a foreign peer collected")
	}
	if _, _, err := reg.Receipt(executor.ReferenceOf(a)); err == nil {
		t.Error("a refused delivery was recorded")
	}
	if _, _, err := col2.Registry.Receipt(executor.ReferenceOf(a2)); err == nil {
		t.Error("a foreign-peer delivery was recorded")
	}
	lane3, _, a3, _ := execWorld(t, executor.ClaudeCode)
	col3 := lane3.d.Executors
	if err := os.WriteFile(filepath.Join(col3.Spool, "result.json"), []byte(`{"schema":"bogus"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := lane3.Ops()[NameExecutorCollect].Run(context.Background(), engine.OpRequest{RunID: "run-1", Args: arg}); err == nil {
		t.Error("a malformed result was received")
	}
	if _, _, err := col3.Registry.Receipt(executor.ReferenceOf(a3)); err == nil {
		t.Error("a malformed result produced a receipt")
	}
}

func TestExecutorCollectThroughEngine(t *testing.T) {
	for _, kind := range []executor.Kind{executor.Codex, executor.Devin, executor.ClaudeCode} {
		t.Run(string(kind), func(t *testing.T) {
			lane, j, a, _ := execWorld(t, kind)
			prog, err := script.Load(script.Source{Name: "executor-collect.js", Text: []byte("export const meta = { name: 'executor-collect' }\nreturn await crw.executorCollect('exec-1')")})
			if err != nil {
				t.Fatal(err)
			}
			res, err := engine.Run(context.Background(), prog, engine.Config{RunID: "run-1", Backend: engine.NewFake(), Policy: engine.FakePolicy(), Journal: j, Ops: lane.Ops()})
			if err != nil {
				t.Fatal(err)
			}
			if res.Status != engine.RunCompleted {
				t.Fatalf("run %s: %s", res.Status, res.Error)
			}
			var completed []engine.Event
			for _, ev := range j.Events() {
				if ev.Kind != engine.KindCall {
					continue
				}
				if ev.Op != OpExecutorCollect {
					t.Errorf("an unrelated call appeared: %+v", ev)
				}
				if ev.Op == OpExecutorCollect && ev.Event == engine.EvCompleted {
					completed = append(completed, ev)
				}
			}
			if len(completed) != 1 {
				t.Fatalf("want one completed collector call, got %d", len(completed))
			}
			head := strings.Repeat("1", 40)
			criteria := a.Task.Identity.CriteriaDigest
			command := strings.Join(a.Task.Checks[0].Args, " ")
			for name, v := range map[string]Verdict{
				"review": ReviewProof(j.Events(), "n1", head, criteria),
				"verify": VerifyProof(j.Events(), "n1", head, command),
				"accept": AcceptProof(j.Events(), "n1", head, func(string) (Acceptance, bool) { return Acceptance{}, false }),
				"final":  ReviewerFinalProof(j.Events(), a.Task.Repository.BaseCommit+".."+head, criteria),
			} {
				if v.Proven {
					t.Errorf("%s proof passed on a collect-only journal", name)
				}
			}
			op := lane.Ops()[NameExecutorCollect]
			if ok, why, err := op.Proven(context.Background(), j.Events(), completed[0]); err != nil || !ok {
				t.Fatalf("stored collector receipt not proven: %v %q %v", ok, why, err)
			}
			next := a.Task
			next.Identity.ExecutionID = "exec-2"
			next.Identity.Attempt++
			next.Identity.AssignmentGeneration++
			admission, err := executor.Freeze(next)
			if err != nil {
				t.Fatal(err)
			}
			grant := executor.Grant{ExecutionID: next.Identity.ExecutionID, AssignmentGeneration: next.Identity.AssignmentGeneration,
				Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
			if _, err := lane.d.Executors.Registry.Admit(admission, grant); err != nil {
				t.Fatal(err)
			}
			if ok, _, err := op.Proven(context.Background(), j.Events(), completed[0]); err != nil || ok {
				t.Errorf("superseded collector receipt still proven: %v %v", ok, err)
			}
		})
	}
}
