package dagsched

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func hostControlKit(t *testing.T) (*releaseKit, capacity.Assignment) {
	t.Helper()
	k := newReleaseKit(t)
	h := enrolled(t, k, "host-a", 2)
	k.putPlan("p", 0, "r", addRelNode("I", dag.NodeImplementation))
	if _, err := k.sched.DeclareRegions(context.Background(), "p", "I", "parent", []Region{region("x.go", "file", "")}); err != nil {
		t.Fatal(err)
	}
	a, err := k.sched.ReleaseExecutor(context.Background(), "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h))
	if err != nil {
		t.Fatal(err)
	}
	return k, a
}
func TestHostAssignmentControlSurvivesHandover(t *testing.T) {
	k, a := hostControlKit(t)
	k.replaceParent("parent-2")
	next := k.session("p", "parent-2", "session-2")
	adopted, err := next.Adopt(context.Background(), "p", "I", "parent-2")
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.Admission.Task.Identity.ExecutionID, adopted.ExecutionID} {
		got, err := next.ExecutorAction(context.Background(), id, "parent-2", "collect", executor.Port{})
		if err != nil {
			t.Fatal(err)
		}
		if got.Admission.TaskDigest != a.Admission.TaskDigest || got.Grant.Owner != "parent-2" || got.Grant.OwnerEpoch <= a.Grant.OwnerEpoch {
			t.Fatalf("handover rewrote work or kept stale authority: %+v", got)
		}
	}
	if _, err := k.sched.ExecutorAction(context.Background(), a.Admission.Task.Identity.ExecutionID, "parent", "collect", executor.Port{}); err == nil {
		t.Fatal("old owner controls adopted execution")
	}
}

type controlAdapter struct{}

func (controlAdapter) Kind() executor.Kind { return executor.Devin }
func (controlAdapter) Capabilities() executor.Capabilities {
	return executor.Capabilities{Revision: "cap-1", Supported: []executor.Capability{executor.CapStart, executor.CapInspect, executor.CapCancel, executor.CapCollect}}
}
func (controlAdapter) Start(_ context.Context, a executor.Admission, _ executor.Grant) (executor.Handle, error) {
	return executor.Handle{Reference: executor.ReferenceOf(a), NativeSession: executor.Observation{Value: "native"}}, nil
}
func (controlAdapter) Inspect(context.Context, executor.Reference) (executor.Inspection, error) {
	return executor.Inspection{State: "running", NativeSession: executor.Observation{Value: "native"}}, nil
}
func (controlAdapter) Cancel(context.Context, executor.Reference, executor.Grant) (executor.Cancellation, error) {
	return executor.Cancellation{Terminated: true, Proof: "whole execution reaped"}, nil
}
func (controlAdapter) Collect(context.Context, executor.Reference) (executor.Delivery, error) {
	return executor.Delivery{ResultPath: "result.json"}, nil
}
func TestHostExpiredGrantCanBeCancelledByCurrentOwner(t *testing.T) {
	k, a := hostControlKit(t)
	port := executor.Port{Adapter: controlAdapter{}, Now: func() time.Time { return a.Grant.ExpiresAt.Add(time.Second) }}
	got, err := k.sched.ExecutorAction(context.Background(), a.Admission.Task.Identity.ExecutionID, "parent", "cancel", port)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != "terminated" || !got.Grant.ExpiresAt.After(port.Now()) {
		t.Fatalf("cancel failed to renew control authority: %+v", got)
	}
}
func TestHostLocalMappingNeverUsesRemoteCheckoutPath(t *testing.T) {
	k := newReleaseKit(t)
	local := enrolled(t, k, "local", 2)
	root := local.Repositories["owner/repo"]
	if err := os.MkdirAll(root, 0700); err != nil {
		t.Fatal(err)
	}
	remote := local
	remote.ID = "aaa-remote"
	remote.Transport = "ssh"
	remote.Endpoint = "approved-remote"
	remote.KnownHosts = filepath.Join(t.TempDir(), "known_hosts")
	remote.Repositories = map[string]string{"owner/other": root}
	if err := (&capacity.HostPool{Store: k.s}).Add(context.Background(), remote); err != nil {
		t.Fatal(err)
	}
	k.putPlan("left", 0, "r", addRelNode("I", dag.NodeImplementation))
	k.putPlan("right", 0, "r", addRelNode("J", dag.NodeImplementation))
	left := region("x.go", "file", "")
	left.Repository = root
	if _, err := k.sched.DeclareRegions(context.Background(), "left", "I", "parent", []Region{left}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.sched.DeclareRegions(context.Background(), "right", "J", "parent", []Region{region("x.go", "file", "")}); err != nil {
		t.Fatal(err)
	}
	if _, err := k.releaseAgent("left", "I"); err != nil {
		t.Fatal(err)
	}
	if _, err := k.sched.ReleaseExecutor(context.Background(), "right", "J", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(local)); err == nil {
		t.Fatal("remote path mapping hid a local region conflict")
	}
}

func TestHostLocalMappingRejectsAmbiguousEnrollment(t *testing.T) {
	k, _ := hostControlKit(t)
	pool := &capacity.HostPool{Store: k.s}
	status, err := pool.Status(context.Background(), "host-a")
	if err != nil {
		t.Fatal(err)
	}
	other := status.Host
	other.ID = "aaa-other-local"
	root := other.Repositories["owner/repo"]
	other.Repositories = map[string]string{"owner/other": root}
	if err = pool.Add(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	r := region("x.go", "file", "")
	r.Repository = root
	if err = k.sched.crossHostRegions(context.Background(), k.s.Querier(context.Background()), "other-plan", "N", []Region{r}); err == nil {
		t.Fatal("ambiguous controller-local identity accepted")
	}
}
