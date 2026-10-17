package dagsched

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/capacity"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

func enrolled(t *testing.T, k *releaseKit, id string, sessions int) crwconfig.ExecutionHost {
	t.Helper()
	p := &capacity.HostPool{Store: k.s}
	h := crwconfig.ExecutionHost{ID: id, Transport: "local", Root: filepath.Join(t.TempDir(), "worker"), Repositories: map[string]string{"owner/repo": filepath.Join(t.TempDir(), "checkout")}, Kind: executor.Devin, CapabilityRevision: "cap-1", LaunchProfile: strings.Repeat("c", 64), ToolchainProfile: strings.Repeat("d", 64), AuthProfile: "approved", TrustProfile: "approved", Supervision: "worker-rpc"}
	if err := p.Add(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	if err := p.Check(context.Background(), capacity.HostObservation{HostID: id, Incarnation: "inc-1", BootID: "boot-" + id, Sequence: 1, EffectiveCPU: 8, AvailableBytes: 32 << 30, Measured: true, Supervised: true, LaunchProfile: h.LaunchProfile, ToolchainProfile: h.ToolchainProfile, CapabilityRevision: h.CapabilityRevision}); err != nil {
		t.Fatal(err)
	}
	if err := p.Enable(context.Background(), id, crwconfig.HostCapacity{Sessions: sessions, TestSlots: 1, TestCPU: 2}); err != nil {
		t.Fatal(err)
	}
	return h
}
func placement(h crwconfig.ExecutionHost) ExecutorPlacement {
	return ExecutorPlacement{Task: executor.Task{Executor: executor.Binding{Kind: h.Kind, CapabilityRevision: h.CapabilityRevision, LaunchProfileDigest: h.LaunchProfile, ToolchainProfileDigest: h.ToolchainProfile}, Requested: executor.Requested{Model: "synthetic", Effort: "high"}, Input: executor.Input{Kind: "prompt", Prompt: "work", BriefDigest: executor.Digest([]byte("work"))}, Checks: []executor.Check{{ID: "test", Args: []string{"go", "test", "./..."}, Resources: executor.Resources{CPU: 1, Heavy: true}}}}, GrantDuration: time.Hour}
}
func TestAssignedReleaseRollsBackDAGReservation(t *testing.T) {
	k := newReleaseKit(t)
	h := enrolled(t, k, "host-a", 1)
	for _, node := range []string{"I", "J"} {
		k.putPlan("p"+node, 0, "r", addRelNode(node, dag.NodeImplementation))
		if _, err := k.sched.DeclareRegions(context.Background(), "p"+node, node, "parent", []Region{region(node+".go", "file", "")}); err != nil {
			t.Fatal(err)
		}
	}
	first, err := k.sched.ReleaseExecutor(context.Background(), "pI", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = k.sched.ReleaseExecutor(context.Background(), "pJ", "J", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h)); err == nil {
		t.Fatal("last host session admitted twice")
	}
	if got := k.heldSlots(); got != 1 {
		t.Fatalf("global slots=%d", got)
	}
	if got := k.count("SELECT COUNT(*) FROM dag_agent_executions"); got != 1 {
		t.Fatalf("orphan execution=%d", got)
	}
	// A flow cancel cannot return the node's global slot while the host may run.
	if _, err = k.sched.AgentClose(context.Background(), "pI", "I", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel}); err == nil {
		t.Fatal("cancel without termination returned global slot")
	}
	p := &capacity.HostPool{Store: k.s}
	if err = p.ObserveExecution(context.Background(), executor.ReferenceOf(first.Admission), executor.Inspection{State: "terminated", Proof: "synthetic process group reaped", NativeSession: executor.Observation{Value: "session"}}); err != nil {
		t.Fatal(err)
	}
	if _, err = k.sched.AgentClose(context.Background(), "pI", "I", "parent", AgentCloseInput{Reason: AgentReasonFlowCancel}); err != nil {
		t.Fatal(err)
	}
}
func TestAssignedHostsCannotBypassFileHotspotOrUnknownRegions(t *testing.T) {
	for _, tc := range []struct {
		name        string
		left, right []Region
	}{{"file", []Region{region("x.go", "file", "")}, []Region{region("x.go", "file", "")}}, {"hotspot", []Region{region("go.mod", "symbol", "one")}, []Region{region("go.mod", "symbol", "two")}}, {"unknown", []Region{region("x.go", "file", "")}, nil}} {
		t.Run(tc.name, func(t *testing.T) {
			k := newReleaseKit(t)
			h := enrolled(t, k, "host-a", 1)
			enrolled(t, k, "host-b", 1)
			k.putPlan("left", 0, "r1", addRelNode("I", dag.NodeImplementation))
			k.putPlan("right", 0, "r1", addRelNode("J", dag.NodeImplementation))
			for _, d := range []struct {
				plan, node string
				regions    []Region
			}{{"left", "I", tc.left}, {"right", "J", tc.right}} {
				if d.regions != nil {
					if _, err := k.sched.DeclareRegions(context.Background(), d.plan, d.node, "parent", d.regions); err != nil {
						t.Fatal(err)
					}
				}
			}
			if _, err := k.sched.ReleaseExecutor(context.Background(), "left", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h)); err != nil {
				t.Fatal(err)
			}
			if _, err := k.sched.ReleaseExecutor(context.Background(), "right", "J", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h)); err == nil {
				t.Fatal("second host bypassed region ownership")
			}
			if k.heldSlots() != 1 {
				t.Fatal("refused release leaked global slot")
			}
		})
	}
}

func TestHostCountDoesNotMultiplyStandingCap(t *testing.T) {
	k := newReleaseKit(t)
	h := enrolled(t, k, "host-a", 10)
	enrolled(t, k, "host-b", 10)
	for i := 0; i < StandingCap+1; i++ {
		node := fmt.Sprintf("N%d", i)
		plan := "plan-" + node
		k.putPlan(plan, 0, "r", addRelNode(node, dag.NodeImplementation))
		if _, err := k.sched.DeclareRegions(context.Background(), plan, node, "parent", []Region{region(node+".go", "file", "")}); err != nil {
			t.Fatal(err)
		}
		_, err := k.sched.ReleaseExecutor(context.Background(), plan, node, "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h))
		if i < StandingCap && err != nil {
			t.Fatal(err)
		}
		if i == StandingCap && err == nil {
			t.Fatal("host count multiplied standing cap")
		}
	}
	if k.heldSlots() != StandingCap {
		t.Fatal("standing cap changed")
	}
}

func TestLegacyReleaseCannotBypassManagedHostRegions(t *testing.T) {
	k := newReleaseKit(t)
	h := enrolled(t, k, "host-a", 2)
	for _, n := range []string{"I", "J"} {
		k.putPlan(n, 0, "r", addRelNode(n, dag.NodeImplementation))
		if _, err := k.sched.DeclareRegions(context.Background(), n, n, "parent", []Region{region("x.go", "file", "")}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := k.sched.ReleaseExecutor(context.Background(), "I", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, placement(h)); err != nil {
		t.Fatal(err)
	}
	if _, err := k.releaseAgent("J", "J"); err == nil {
		t.Fatal("legacy release bypassed managed region holder")
	}
}

func TestPlacementFreezesSelectedApprovedHostProfiles(t *testing.T) {
	k := newReleaseKit(t)
	h := enrolled(t, k, "host-a", 1)
	k.putPlan("p", 0, "r", addRelNode("I", dag.NodeImplementation))
	if _, err := k.sched.DeclareRegions(context.Background(), "p", "I", "parent", []Region{region("x.go", "file", "")}); err != nil {
		t.Fatal(err)
	}
	in := placement(h)
	in.Task.Executor.LaunchProfileDigest = ""
	in.Task.Executor.ToolchainProfileDigest = ""
	a, err := k.sched.ReleaseExecutor(context.Background(), "p", "I", "parent", k.agentRequest(true), AgentReleaseOptions{}, in)
	if err != nil {
		t.Fatal(err)
	}
	if a.Admission.Task.Executor.LaunchProfileDigest != h.LaunchProfile || a.Admission.Task.Executor.ToolchainProfileDigest != h.ToolchainProfile || executor.ValidateAdmission(a.Admission) != nil {
		t.Fatal("selected profiles not frozen")
	}
}
