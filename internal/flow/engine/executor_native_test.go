package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

type codexKindFake struct{ *Fake }

func (codexKindFake) ExecutorKind() executor.Kind { return executor.Codex }

type otherKindFake struct{ *Fake }

func (otherKindFake) ExecutorKind() executor.Kind { return executor.Devin }

func enTask(node, prompt string, pair Pair) executor.Task {
	return executor.Task{
		Schema:   executor.TaskSchema,
		Identity: executor.Identity{TaskID: "task-1", ExecutionID: "exec-1", Attempt: 1, AssignmentGeneration: 1, Plan: "plan-1", Node: node, Revision: "r0", CriteriaDigest: strings.Repeat("a", 64)},
		Repository: executor.Repository{Forge: "github", ID: "repo-1", BaseCommit: strings.Repeat("b", 40),
			TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/" + node},
		Regions:  []executor.Region{{Path: "x/f.go"}},
		WorkKind: "implementation",
		Executor: executor.Binding{HostID: "host-1", Incarnation: "inc-1", Kind: executor.Codex, CapabilityRevision: "cap-1",
			LaunchProfileDigest: strings.Repeat("c", 64), ToolchainProfileDigest: strings.Repeat("d", 64)},
		Requested: executor.Requested{Model: pair.Model, Effort: pair.Effort},
		Input:     executor.Input{Kind: "prompt", Prompt: prompt, BriefDigest: executor.Digest([]byte(prompt))},
		Checks:    []executor.Check{{ID: "check-1", Args: []string{"true"}, Resources: executor.Resources{CPU: 1}}},
	}
}

func enResolver(task executor.Task, err error) func(context.Context, SnapshotRequest) (executor.Admission, error) {
	return func(context.Context, SnapshotRequest) (executor.Admission, error) {
		if err != nil {
			return executor.Admission{}, err
		}
		return executor.Freeze(task)
	}
}

func enRows(j *MemoryJournal, event string) []Event {
	var out []Event
	for _, ev := range j.Events() {
		if ev.Event == event {
			out = append(out, ev)
		}
	}
	return out
}

func TestExecutorNativeProvenance(t *testing.T) {
	body := `const b = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'build' })
return b`
	t.Run("admission and provenance recorded", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Backend = codexKindFake{h.fake}
		pair := Pair{Model: "claude-fake-haiku", Effort: "xhigh"}
		task := enTask("n1", "build it", pair)
		h.cfg.ExecutorAdmission = enResolver(task, nil)
		res := h.run(body)
		got := value[map[string]any](t, res)
		if got["status"] != "completed" {
			t.Fatalf("%v", got)
		}
		prepared := enRows(h.journal, EvPrepared)
		if len(prepared) != 1 {
			t.Fatalf("%d prepared rows", len(prepared))
		}
		raw, _ := json.Marshal(prepared[0].Data["executor_admission"])
		var adm map[string]any
		if err := json.Unmarshal(raw, &adm); err != nil || adm["task_digest"] == nil {
			t.Fatalf("no executor_admission in prepared row: %+v", prepared[0].Data)
		}
		task2, _ := executor.Freeze(task)
		if adm["task_digest"] != task2.TaskDigest {
			t.Errorf("admission digest %v", adm["task_digest"])
		}
		natives := enRows(h.journal, "executor_native")
		if len(natives) != 1 {
			t.Fatalf("%d executor_native rows", len(natives))
		}
		row := natives[0]
		if row.Kind != KindFlow || row.Op != "executor" {
			t.Errorf("row %+v", row)
		}
		praw, _ := json.Marshal(row.Data["provenance"])
		var prov map[string]any
		if err := json.Unmarshal(praw, &prov); err != nil || prov == nil {
			t.Fatalf("no provenance: %+v", row.Data)
		}
		var b CallResult
		for _, c := range res.Calls {
			if c.Label == "build" {
				b = c
			}
		}
		if sess, _ := prov["native_session"].(map[string]any); sess["value"] != b.Meta.ThreadID {
			t.Errorf("native session %v vs thread %s", sess, b.Meta.ThreadID)
		}
		turns, _ := prov["native_turns"].([]any)
		if len(turns) != len(b.Meta.TurnIDs) {
			t.Errorf("native turns %v vs %v", turns, b.Meta.TurnIDs)
		}
		for i, tn := range turns {
			if tn != b.Meta.TurnIDs[i] {
				t.Errorf("turn %d: %v vs %s", i, tn, b.Meta.TurnIDs[i])
			}
		}
		if m, _ := prov["model"].(map[string]any); m["value"] != "claude-fake-haiku" {
			t.Errorf("model %v", m)
		}
		if row.Data["terminal_event"] != EvCompleted {
			t.Errorf("terminal_event %v", row.Data["terminal_event"])
		}
		if row.Data["native_call_seq"] == nil {
			t.Errorf("no native_call_seq")
		}
	})
	t.Run("nil callback leaves behavior unchanged", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Backend = codexKindFake{h.fake}
		res := h.run(body)
		got := value[map[string]any](t, res)
		if got["status"] != "completed" {
			t.Fatalf("%v", got)
		}
		if len(enRows(h.journal, "executor_native")) != 0 {
			t.Error("executor rows without a callback")
		}
		if adm := enRows(h.journal, EvPrepared)[0].Data["executor_admission"]; adm != nil {
			t.Error("executor_admission without a callback")
		}
	})
	t.Run("refusals before dispatch", func(t *testing.T) {
		for name, mut := range map[string]func(*harness){
			"resolver error": func(h *harness) {
				h.cfg.ExecutorAdmission = enResolver(enTask("n1", "build it", Pair{Model: "claude-fake-haiku", Effort: "xhigh"}), errors.New("no envelope"))
			},
			"wrong kind": func(h *harness) {
				h.cfg.Backend = otherKindFake{h.fake}
				h.cfg.ExecutorAdmission = enResolver(enTask("n1", "build it", Pair{Model: "claude-fake-haiku", Effort: "xhigh"}), nil)
			},
			"wrong node": func(h *harness) {
				h.cfg.ExecutorAdmission = enResolver(enTask("n9", "build it", Pair{Model: "claude-fake-haiku", Effort: "xhigh"}), nil)
			},
			"wrong prompt": func(h *harness) {
				h.cfg.ExecutorAdmission = enResolver(enTask("n1", "other", Pair{Model: "claude-fake-haiku", Effort: "xhigh"}), nil)
			},
			"wrong requested": func(h *harness) {
				h.cfg.ExecutorAdmission = enResolver(enTask("n1", "build it", Pair{Model: "other", Effort: "low"}), nil)
			},
		} {
			t.Run(name, func(t *testing.T) {
				h := newHarness(t)
				if _, ok := h.cfg.Backend.(*Fake); ok {
					h.cfg.Backend = codexKindFake{h.fake}
				}
				mut(h)
				res := h.run(body)
				got := value[map[string]any](t, res)
				if got["reason"] != "host_config_executor_envelope" {
					t.Fatalf("%v", got)
				}
				if len(h.fake.Requests()) != 0 {
					t.Errorf("the refusal still sent %d requests", len(h.fake.Requests()))
				}
			})
		}
	})
	t.Run("task digest joins replay identity", func(t *testing.T) {
		h := newHarness(t)
		h.cfg.Backend = codexKindFake{h.fake}
		pair := Pair{Model: "claude-fake-haiku", Effort: "xhigh"}
		h.cfg.ExecutorAdmission = enResolver(enTask("n1", "build it", pair), nil)
		res := h.run(body)
		if got := value[map[string]any](t, res); got["status"] != "completed" {
			t.Fatalf("%v", got)
		}
		var b1 CallResult
		for _, c := range res.Calls {
			if c.Label == "build" {
				b1 = c
			}
		}
		h2 := newHarness(t)
		h2.cfg.Backend = codexKindFake{h2.fake}
		h2.cfg.Journal = h.journal
		h2.cfg.Resume = &ResumeOptions{}
		h2.cfg.ExecutorAdmission = enResolver(enTask("n1", "build it", pair), nil)
		res2 := h2.run(body)
		if got := value[map[string]any](t, res2); got["status"] != "completed" {
			t.Fatalf("%v", got)
		}
		if len(h2.fake.Requests()) != 0 {
			t.Errorf("the replay ran %d new requests", len(h2.fake.Requests()))
		}
		var b2 CallResult
		for _, c := range res2.Calls {
			if c.Label == "build" {
				b2 = c
			}
		}
		if b2.Meta.ThreadID != b1.Meta.ThreadID || len(b2.Meta.TurnIDs) != len(b1.Meta.TurnIDs) {
			t.Errorf("the replay changed the native ids: %+v vs %+v", b2.Meta, b1.Meta)
		}
		if n := len(enRows(h.journal, "executor_native")); n != 1 {
			t.Errorf("%d provenance rows after the replay", n)
		}
		h3 := newHarness(t)
		h3.cfg.Backend = codexKindFake{h3.fake}
		h3.cfg.Journal = h.journal
		h3.cfg.Resume = &ResumeOptions{}
		other := enTask("n1", "build it", pair)
		other.Identity.ExecutionID = "exec-2"
		h3.cfg.ExecutorAdmission = enResolver(other, nil)
		res3 := h3.run(body)
		got3 := value[map[string]any](t, res3)
		if len(h3.fake.Requests()) == 0 && got3["status"] != "refused" {
			t.Errorf("a different task digest replayed the old call: %v", got3)
		}
	})
}

func TestExecutorNativeSessionProvenance(t *testing.T) {
	k := newSessionKit(t)
	pair := Pair{Model: "claude-fake-haiku", Effort: "xhigh"}
	task := enTask("n1", "build node n1", pair)
	called := 0
	k.h.cfg.ExecutorAdmission = func(context.Context, SnapshotRequest) (executor.Admission, error) {
		called++
		return executor.Freeze(task)
	}
	res := k.h.run(`const b = await agent.call('build node n1', { agentType: 'executor', node: 'n1', label: 'build' })
const q = await agent.call('check it', { agentType: 'executor', node: 'n1', label: 'qa', qa: true })
return b`)
	got := value[map[string]any](t, res)
	if got["status"] != "completed" {
		t.Fatalf("%v", got)
	}
	if called != 1 {
		t.Errorf("the resolver ran %d times for one executor call and a qa call", called)
	}
	natives := enRows(k.h.journal, "executor_native")
	if len(natives) != 1 {
		t.Fatalf("%d executor_native rows", len(natives))
	}
	var b CallResult
	for _, c := range res.Calls {
		if c.Label == "build" {
			b = c
		}
	}
	praw, _ := json.Marshal(natives[0].Data["provenance"])
	var prov map[string]any
	if err := json.Unmarshal(praw, &prov); err != nil {
		t.Fatal(err)
	}
	if sess, _ := prov["native_session"].(map[string]any); sess["value"] != b.Meta.ThreadID {
		t.Errorf("session %v vs the call's thread %s", sess, b.Meta.ThreadID)
	}
	var startRows []Event
	for _, ev := range k.h.journal.Events() {
		if ev.Event == EvThreadStarted && ev.CallID == b.CallID {
			startRows = append(startRows, ev)
		}
	}
	if len(startRows) == 0 || fmt.Sprint(startRows[0].Data["thread_id"]) != b.Meta.ThreadID {
		t.Errorf("the provenance names a thread the journal never started: %v", startRows)
	}
	turns, _ := prov["native_turns"].([]any)
	if len(turns) != len(b.Meta.TurnIDs) {
		t.Fatalf("turns %v vs %v", turns, b.Meta.TurnIDs)
	}
	for i, tn := range turns {
		if tn != b.Meta.TurnIDs[i] {
			t.Errorf("turn %d: %v vs %s", i, tn, b.Meta.TurnIDs[i])
		}
	}
	for _, req := range k.placer.Requests() {
		if req.Kind == "thread" || req.Kind == "turn" {
			t.Errorf("the session path sent a backend request: %+v", req)
		}
	}
}

func TestExecutorAdmissionFailurePreservesRecoveryHold(t *testing.T) {
	body := `return await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'build' })`
	h := newHarness(t)
	h.cfg.Backend = codexKindFake{h.fake}
	pair := Pair{Model: "claude-fake-haiku", Effort: "xhigh"}
	h.cfg.ExecutorAdmission = enResolver(enTask("n1", "build it", pair), nil)
	h.fake.Handler = func(FakeRequest) FakeReply {
		return FakeReply{Err: &TransportError{Err: errors.New("connection lost")}}
	}
	res := h.run(body)
	if got := value[map[string]any](t, res); got["status"] != "unknown" {
		t.Fatalf("initial status: %v", got)
	}
	var oldCall CallResult
	for _, c := range res.Calls {
		if c.Label == "build" {
			oldCall = c
		}
	}
	preparedBefore := len(enRows(h.journal, EvPrepared))
	resolverCalls := 0
	h2 := newHarness(t)
	h2.cfg.Backend = codexKindFake{h2.fake}
	h2.cfg.Journal = h.journal
	h2.cfg.Resume = &ResumeOptions{}
	h2.cfg.ExecutorAdmission = func(context.Context, SnapshotRequest) (executor.Admission, error) {
		resolverCalls++
		return executor.Admission{}, errors.New("admission lookup unavailable")
	}
	res2 := h2.run(body)
	if got := value[map[string]any](t, res2); got["status"] != "unknown" {
		t.Fatalf("a failing resolver turned the held call into %v", got)
	}
	if len(h2.fake.Requests()) != 0 {
		t.Errorf("the failed lookup sent %d requests", len(h2.fake.Requests()))
	}
	if resolverCalls != 0 {
		t.Errorf("the resolver ran %d times for a held call", resolverCalls)
	}
	holds := RecoveryHolds(h.journal, "run-test")
	found := false
	for _, id := range holds["n1"] {
		if id == oldCall.CallID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unresolved call lost its recovery hold: %+v", holds)
	}
	for _, ev := range enRows(h.journal, EvRefused) {
		if ev.CallID == oldCall.CallID {
			t.Errorf("the held call was refused: %+v", ev)
		}
	}
	if len(enRows(h.journal, EvPrepared)) != preparedBefore {
		t.Errorf("a held call wrote a new prepared row")
	}
	h3 := newHarness(t)
	h3.cfg.Backend = codexKindFake{h3.fake}
	h3.cfg.Journal = h.journal
	h3.cfg.Resume = &ResumeOptions{}
	h3.cfg.ExecutorAdmission = h.cfg.ExecutorAdmission
	res3 := h3.run(`const old = await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'build' }); return await agent.call('build it', { agentType: 'executor', node: 'n1', label: 'retry' });`)
	got3 := value[map[string]any](t, res3)
	if len(h3.fake.Requests()) != 0 {
		t.Errorf("a restored resolver sent %d requests without resolving the old outcome", len(h3.fake.Requests()))
	}
	var old3, retry CallResult
	for _, c := range res3.Calls {
		if c.Label == "build" {
			old3 = c
		}
		if c.Label == "retry" {
			retry = c
		}
	}
	if old3.Status != "unknown" {
		t.Errorf("the held call became %s", old3.Status)
	}
	if retry.Status != "held" || retry.Reason != ReasonRecoveryHold {
		t.Errorf("the fresh call was not held by the unresolved one: %v", got3)
	}
	if n := len(enRows(h.journal, "executor_native")); n != 1 {
		t.Errorf("%d provenance rows; only the original unknown call's row belongs", n)
	}
	holds = RecoveryHolds(h.journal, "run-test")
	if ids := holds["n1"]; len(ids) != 1 || ids[0] != oldCall.CallID {
		t.Errorf("the original recovery hold was not preserved: %v", ids)
	}
}
