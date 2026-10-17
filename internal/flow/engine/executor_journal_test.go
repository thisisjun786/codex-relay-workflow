package engine

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

func ejTask() executor.Task {
	prompt := "do n1"
	return executor.Task{
		Schema:   executor.TaskSchema,
		Identity: executor.Identity{TaskID: "task-1", ExecutionID: "exec-1", Attempt: 1, AssignmentGeneration: 1, Plan: "plan-1", Node: "n1", Revision: "r0", CriteriaDigest: strings.Repeat("a", 64)},
		Repository: executor.Repository{Forge: "github", ID: "repo-1", BaseCommit: strings.Repeat("b", 40),
			TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/n1"},
		Regions:  []executor.Region{{Path: "x/f.go"}},
		WorkKind: "implementation",
		Executor: executor.Binding{HostID: "host-1", Incarnation: "inc-1", Kind: executor.Codex, CapabilityRevision: "cap-1",
			LaunchProfileDigest: strings.Repeat("c", 64), ToolchainProfileDigest: strings.Repeat("d", 64)},
		Requested: executor.Requested{Model: "m", Effort: "high"},
		Input:     executor.Input{Kind: "prompt", Prompt: prompt, BriefDigest: executor.Digest([]byte(prompt))},
		Checks:    []executor.Check{{ID: "check-1", Args: []string{"true"}, Resources: executor.Resources{CPU: 1}}},
	}
}

func ejWorld(t *testing.T) (*executor.Registry, *ExecutorLedger, executor.Admission, string) {
	t.Helper()
	j := NewMemoryJournal()
	l := NewExecutorLedger(j, "run-1")
	reg := executor.NewRegistry(l, func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) })
	a, err := executor.Freeze(ejTask())
	if err != nil {
		t.Fatal(err)
	}
	spool := filepath.Join(t.TempDir(), "spool")
	if err := os.Mkdir(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	return reg, l, a, spool
}

func ejResultFile(t *testing.T, a executor.Admission, spool string) {
	t.Helper()
	r, err := executor.NewResult(a, "completed", executor.Payload{Narrative: "done",
		Candidate: &executor.Candidate{Branch: a.Task.Repository.TaskBranch, Head: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40)},
		Checks:    []executor.CheckResult{{ID: "check-1", ExitCode: 0, Outcome: "passed"}},
		Artifacts: []executor.Artifact{}},
		executor.Provenance{Provider: a.Task.Executor.Kind, NativeSession: executor.Observation{Value: "s"},
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
}

func TestExecutorLedgerRoundTrip(t *testing.T) {
	reg, l, a, spool := ejWorld(t)
	g := executor.Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1,
		ExpiresAt: time.Date(2026, 10, 17, 13, 0, 0, 0, time.UTC)}
	ref, err := reg.Admit(a, g)
	if err != nil {
		t.Fatalf("admit: %v", err)
	}
	ejResultFile(t, a, spool)
	peer := executor.Peer{HostID: "host-1", Incarnation: "inc-1"}
	ack, err := reg.Collect(ref, spool, "result.json", peer, executor.DefaultLimits)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	j := l.journal.(*MemoryJournal)
	native := Event{RunID: "run-1", Kind: KindCall, CallID: "c1", Op: "agent", Event: EvCompleted, Data: map[string]any{"x": "y"}}
	j.Append(native)
	j.Append(Event{RunID: "other", Kind: KindFlow, Op: "executor", Event: "executor_record", Data: map[string]any{"record": `{"kind":"bogus"}`}})
	reg2 := executor.NewRegistry(NewExecutorLedger(j, "run-1"), func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) })
	ack2, err := reg2.Collect(ref, spool, "result.json", peer, executor.DefaultLimits)
	if err != nil || ack2 != ack {
		t.Fatalf("a rebuilt ledger did not replay the acknowledgement: %v %+v", err, ack2)
	}
	for _, ev := range j.Events() {
		if ev.Op == "executor" && ev.Event == "executor_record" && ev.Kind != KindFlow {
			t.Errorf("an executor record was written outside the flow kind")
		}
		if ev.Kind == KindCall && ev.CallID == "exec-1" {
			t.Errorf("an executor wrote a call row for its execution")
		}
	}
	if _, ok := LookupCall(j, "c1"); !ok {
		t.Error("the foreign agent row was lost")
	}
	var back Event
	for _, ev := range j.Events() {
		if ev.Kind == KindCall && ev.CallID == "c1" {
			back = ev
		}
	}
	native.Seq = back.Seq
	if !reflect.DeepEqual(back, native) {
		t.Errorf("the foreign agent row changed: %+v", back)
	}
	bad := NewExecutorLedger(NewMemoryJournal(), "")
	reg3 := executor.NewRegistry(bad, nil)
	live := executor.Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	if _, err := reg3.Admit(a, live); err == nil {
		t.Error("a ledger with no run id admitted")
	}
	reg4 := executor.NewRegistry(NewExecutorLedger(nil, "run-1"), nil)
	if _, err := reg4.Admit(a, live); err == nil {
		t.Error("a ledger with no journal admitted")
	}
}

func TestExecutorLedgerFailure(t *testing.T) {
	a, _ := executor.Freeze(ejTask())
	g := executor.Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	j := &ejErrJournal{MemoryJournal: NewMemoryJournal(), err: errors.New("store down")}
	reg := executor.NewRegistry(NewExecutorLedger(j, "run-1"), nil)
	if _, err := reg.Admit(a, g); err == nil {
		t.Error("a failing journal admitted")
	}
	fj := &ejFlushJournal{MemoryJournal: NewMemoryJournal(), fail: true}
	l1 := NewExecutorLedger(fj, "run-1")
	reg = executor.NewRegistry(l1, nil)
	if _, err := reg.Admit(a, g); err == nil {
		t.Error("a journal that cannot flush acknowledged an admission")
	}
	fj.fail = false
	if _, err := reg.Admit(a, g); err == nil {
		t.Error("a poisoned ledger recovered silently")
	}
	fj.fail = true
	regFresh := executor.NewRegistry(NewExecutorLedger(fj, "run-1"), nil)
	if _, _, err := regFresh.Current("exec-1"); err == nil {
		t.Error("a fresh wrapper over a still-failing journal answered")
	}
	fj.fail = false
	regHealed := executor.NewRegistry(NewExecutorLedger(fj, "run-1"), nil)
	if _, err := regHealed.Admit(a, g); err != nil {
		t.Fatalf("a fresh wrapper over a healed journal: %v", err)
	}
}

type ejErrJournal struct {
	*MemoryJournal
	err error
}

func (j *ejErrJournal) Err() error { return j.err }

type ejFlushJournal struct {
	*MemoryJournal
	fail        bool
	afterAppend bool
	appended    int
}

func (j *ejFlushJournal) Append(e Event) Event {
	j.appended++
	return j.MemoryJournal.Append(e)
}

func (j *ejFlushJournal) Flush() error {
	if j.fail && (!j.afterAppend || j.appended > 0) {
		return errors.New("no disk")
	}
	return nil
}

func TestExecutorLedgerBufferedRows(t *testing.T) {
	a, _ := executor.Freeze(ejTask())
	g := executor.Grant{ExecutionID: "exec-1", AssignmentGeneration: 1, Owner: "w", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
	fj := &ejFlushJournal{MemoryJournal: NewMemoryJournal(), fail: true, afterAppend: true}
	l1 := NewExecutorLedger(fj, "run-1")
	reg := executor.NewRegistry(l1, nil)
	if _, err := reg.Admit(a, g); err == nil {
		t.Error("a record whose flush failed was acknowledged")
	}
	if _, err := reg.Admit(a, g); err == nil {
		t.Error("a poisoned wrapper recovered")
	}
	reg2 := executor.NewRegistry(NewExecutorLedger(fj, "run-1"), nil)
	if _, _, err := reg2.Current("exec-1"); err == nil {
		t.Error("a buffered row was read before its flush succeeded")
	}
	fj.fail = false
	reg3 := executor.NewRegistry(NewExecutorLedger(fj, "run-1"), nil)
	if _, _, err := reg3.Current("exec-1"); err != nil {
		t.Fatalf("a row flushed by the next preflush was not replayed: %v", err)
	}
}
