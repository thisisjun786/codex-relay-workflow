package worker

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type lostAdapter struct {
	starts int
	lost   bool
}

func (f *lostAdapter) Kind() executor.Kind { return executor.Devin }
func (f *lostAdapter) Capabilities() executor.Capabilities {
	return executor.Capabilities{Revision: "cap-1", Supported: []executor.Capability{executor.CapStart, executor.CapInspect, executor.CapCancel, executor.CapCollect}}
}
func (f *lostAdapter) Start(_ context.Context, a executor.Admission, _ executor.Grant) (executor.Handle, error) {
	f.starts++
	if f.lost {
		return executor.Handle{}, errors.New("reply lost")
	}
	return executor.Handle{Reference: executor.ReferenceOf(a), NativeSession: executor.Observation{Value: "native-1"}}, nil
}
func (f *lostAdapter) Inspect(context.Context, executor.Reference) (executor.Inspection, error) {
	return executor.Inspection{State: "unknown", NativeSession: executor.Observation{UnknownReason: "unreachable"}}, nil
}
func (f *lostAdapter) Cancel(context.Context, executor.Reference, executor.Grant) (executor.Cancellation, error) {
	return executor.Cancellation{}, errors.New("cancel acknowledgement lost")
}
func (f *lostAdapter) Collect(context.Context, executor.Reference) (executor.Delivery, error) {
	return executor.Delivery{}, errors.New("not complete")
}
func task(t *testing.T) (executor.Admission, executor.Grant) {
	t.Helper()
	a, err := executor.Freeze(executor.Task{Schema: executor.TaskSchema, Identity: executor.Identity{TaskID: "t1", ExecutionID: "e1", Attempt: 1, AssignmentGeneration: 1, Plan: "p", Node: "n", Revision: "1", CriteriaDigest: strings.Repeat("a", 64)}, Repository: executor.Repository{Forge: "github", ID: "owner/repo", BaseCommit: strings.Repeat("b", 40), TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/e1"}, Regions: []executor.Region{{Path: "x.go"}}, WorkKind: "implementation", Executor: executor.Binding{HostID: "h", Incarnation: "inc", Kind: executor.Devin, CapabilityRevision: "cap-1", LaunchProfileDigest: strings.Repeat("c", 64), ToolchainProfileDigest: strings.Repeat("d", 64)}, Input: executor.Input{Kind: "prompt", Prompt: "work", BriefDigest: executor.Digest([]byte("work"))}, Requested: executor.Requested{Model: "synthetic", Effort: "high"}, Checks: []executor.Check{{ID: "check", Args: []string{"go", "test", "./..."}, Resources: executor.Resources{CPU: 1, Heavy: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	return a, executor.Grant{ExecutionID: "e1", AssignmentGeneration: 1, Owner: "owner", OwnerEpoch: 1, ExpiresAt: time.Now().Add(time.Hour)}
}
func TestStartReplyLossRestartAndCancelLossDoNotStartAgain(t *testing.T) {
	a, g := task(t)
	root := filepath.Join(t.TempDir(), "journal")
	backend := &lostAdapter{lost: true}
	w, err := Open(root, executor.Peer{HostID: "h", Incarnation: "inc"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Start(context.Background(), a, g); err == nil {
		t.Fatal("lost response answered success")
	}
	_ = w.Close()
	w, err = Open(root, executor.Peer{HostID: "h", Incarnation: "inc"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	backend.lost = false
	if _, err := w.Start(context.Background(), a, g); err == nil {
		t.Fatal("ambiguous start retried")
	}
	if _, err := w.Cancel(context.Background(), executor.ReferenceOf(a), g); err == nil {
		t.Fatal("cancel loss answered success")
	}
	if _, err := w.Start(context.Background(), a, g); err == nil {
		t.Fatal("cancel loss allowed restart")
	}
	if backend.starts != 1 {
		t.Fatalf("%d starts", backend.starts)
	}
}
func TestWorkerIncarnationDoesNotAdoptOldStart(t *testing.T) {
	a, g := task(t)
	backend := &lostAdapter{}
	w, err := Open(filepath.Join(t.TempDir(), "journal"), executor.Peer{HostID: "h", Incarnation: "new-inc"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.Start(context.Background(), a, g); err == nil || backend.starts != 0 {
		t.Fatalf("foreign incarnation: %v starts=%d", err, backend.starts)
	}
}

type terminalAdapter struct{ lostAdapter }

func (*terminalAdapter) Inspect(context.Context, executor.Reference) (executor.Inspection, error) {
	return executor.Inspection{State: "terminated", Proof: "provider reaped", NativeSession: executor.Observation{Value: "native-1"}}, nil
}
func (*terminalAdapter) Cancel(context.Context, executor.Reference, executor.Grant) (executor.Cancellation, error) {
	return executor.Cancellation{Terminated: true, Proof: "provider reaped"}, nil
}
func TestProviderTerminationDoesNotReleaseRunningHeavyGroup(t *testing.T) {
	a, g := task(t)
	w, err := Open(filepath.Join(t.TempDir(), "worker"), executor.Peer{HostID: "h", Incarnation: "inc"}, &terminalAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err = w.Start(context.Background(), a, g); err != nil {
		t.Fatal(err)
	}
	ref := executor.ReferenceOf(a)
	group := Group{Reference: ref, Permit: Permit{ID: executor.Digest([]byte("e1\x00check")), CPU: 1}, Args: a.Task.Checks[0].Args}
	runner := &blockingGroup{entered: make(chan Group, 1), done: make(chan struct{})}
	done := make(chan error, 1)
	go func() { _, err := w.RunGroup(context.Background(), group, runner); done <- err }()
	<-runner.entered
	if c, err := w.Cancel(context.Background(), ref, g); err != nil || c.Terminated {
		t.Fatalf("provider cancelled while tools run: %+v %v", c, err)
	}
	if i, err := w.Inspect(context.Background(), ref); err != nil || i.State != "unknown" {
		t.Fatalf("provider terminal while tools run: %+v %v", i, err)
	}
	close(runner.done)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	if i, err := w.Inspect(context.Background(), ref); err != nil || i.State != "terminated" {
		t.Fatalf("whole execution: %+v %v", i, err)
	}
}

func TestRestartInspectsOldJournalButCannotStartIt(t *testing.T) {
	a, g := task(t)
	root := filepath.Join(t.TempDir(), "worker")
	backend := &terminalAdapter{}
	w, err := Open(root, executor.Peer{HostID: "h", Incarnation: "inc"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = w.Start(context.Background(), a, g); err != nil {
		t.Fatal(err)
	}
	_ = w.Close()
	w, err = Open(root, executor.Peer{HostID: "h", Incarnation: "new-inc"}, backend)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err = w.Start(context.Background(), a, g); err == nil {
		t.Fatal("old incarnation restarted")
	}
	i, err := w.Inspect(context.Background(), executor.ReferenceOf(a))
	if err != nil || i.State != "terminated" {
		t.Fatalf("reconcile persisted execution: %+v %v", i, err)
	}
	if backend.starts != 1 {
		t.Fatal("duplicate provider start")
	}
}

type countingGroups struct{ calls int }

func (f *countingGroups) InspectGroup(context.Context, Group, bool) (CommandResult, error) {
	f.calls++
	return CommandResult{}, nil
}
func TestWorkerRenewalFencesGroupCancellationBeforeEffects(t *testing.T) {
	a, g := task(t)
	w, err := Open(filepath.Join(t.TempDir(), "worker"), executor.Peer{HostID: "h", Incarnation: "inc"}, &terminalAdapter{})
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err = w.Start(context.Background(), a, g); err != nil {
		t.Fatal(err)
	}
	ref := executor.ReferenceOf(a)
	next := g
	next.Owner = "new-owner"
	next.OwnerEpoch++
	next.ExpiresAt = next.ExpiresAt.Add(time.Hour)
	if err = w.RenewGrant(context.Background(), ref, next); err != nil {
		t.Fatal(err)
	}
	group := Group{Reference: ref, Permit: Permit{ID: executor.Digest([]byte("e1\x00check")), CPU: 1}, Args: a.Task.Checks[0].Args}
	if _, err = w.db.Exec("INSERT INTO commands(permit_id,execution_id,group_json,owner_pid,state) VALUES (?,?,?,?,'running')", group.Permit.ID, "e1", jsonText(group), os.Getpid()); err != nil {
		t.Fatal(err)
	}
	runner := &countingGroups{}
	if err = w.ReconcileExecution(context.Background(), ref, &g, runner, true); err == nil || runner.calls != 0 {
		t.Fatal("stale grant reached group cancellation")
	}
	bad := ref
	bad.TaskDigest = strings.Repeat("f", 64)
	if err = w.ReconcileExecution(context.Background(), bad, &next, runner, true); err == nil || runner.calls != 0 {
		t.Fatal("foreign reference reached group cancellation")
	}
	if err = w.CheckProfiles(context.Background(), ref, strings.Repeat("f", 64), a.Task.Executor.ToolchainProfileDigest); err == nil {
		t.Fatal("changed cgroup/launch profile accepted")
	}
	if err = w.ReconcileExecution(context.Background(), ref, &next, runner, true); err != nil || runner.calls != 1 {
		t.Fatalf("new authority cannot cancel groups: %v %d", err, runner.calls)
	}
}
