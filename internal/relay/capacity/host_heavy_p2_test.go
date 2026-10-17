package capacity

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

func crw1650RunningHost(t *testing.T) (*HostPool, executor.Reference) {
	t.Helper()
	p, h := hostPool(t, 1, 4, 4)
	globalTable(t, p)
	a, g := hostAdmission(t, h, "e1")
	if _, err := reserveHost(p, a, g); err != nil {
		t.Fatal(err)
	}
	ref := executor.ReferenceOf(a)
	if err := p.ObserveExecution(context.Background(), ref, executor.Inspection{State: "running", NativeSession: executor.Observation{Value: "synthetic"}}); err != nil {
		t.Fatal(err)
	}
	return p, ref
}

func crw1650NoHeldPermits(t *testing.T, p *HostPool, host string) {
	t.Helper()
	s, err := p.Status(context.Background(), host)
	if err != nil {
		t.Fatal(err)
	}
	if s.HeldHeavy != 0 || s.HeldCPU != 0 {
		t.Errorf("status shows %d held permits / %d held CPU; want none", s.HeldHeavy, s.HeldCPU)
	}
}

func TestCRW1650HeavyPermitUsesLatestCPU(t *testing.T) {
	ctx := context.Background()
	p, ref := crw1650RunningHost(t)
	s, err := p.Status(ctx, ref.Executor.HostID)
	if err != nil {
		t.Fatal(err)
	}
	obs := s.Observation
	obs.Sequence++
	obs.EffectiveCPU = 2
	if err := p.Heartbeat(ctx, obs); err != nil {
		t.Fatal(err)
	}
	s, err = p.Status(ctx, ref.Executor.HostID)
	if err != nil || s.Enabled {
		t.Fatalf("host still enabled after the CPU reduction: %v %+v", err, s)
	}
	if _, err := p.AcquireHeavy(ctx, ref, "too-large", 3); err == nil {
		t.Fatal("a 3-CPU command fit the shrunken host")
	}
	crw1650NoHeldPermits(t, p, ref.Executor.HostID)
}

func TestCRW1650HeavyPermitPreservesHeldWork(t *testing.T) {
	ctx := context.Background()
	p, ref := crw1650RunningHost(t)
	old, err := p.AcquireHeavy(ctx, ref, "old", 1)
	if err != nil {
		t.Fatal(err)
	}
	s, err := p.Status(ctx, ref.Executor.HostID)
	if err != nil {
		t.Fatal(err)
	}
	obs := s.Observation
	obs.Sequence++
	obs.EffectiveCPU = 2
	if err := p.Heartbeat(ctx, obs); err != nil {
		t.Fatal(err)
	}
	again, err := p.AcquireHeavy(ctx, ref, "old", 1)
	if err != nil || again != old {
		t.Fatalf("the held permit was recalled or changed: %+v %v", again, err)
	}
	if _, err := p.AcquireHeavy(ctx, ref, "new-two", 2); err == nil {
		t.Fatal("a new 2-CPU command fit beside the held permit over the shrunken budget")
	}
	if err := p.EndHeavy(ctx, ref, old.ID, "synthetic terminated"); err != nil {
		t.Fatal(err)
	}
	grown, err := p.AcquireHeavy(ctx, ref, "new-two", 2)
	if err != nil || grown.State != "held" {
		t.Fatalf("the freed budget on the draining host: %+v %v", grown, err)
	}
	if err := p.EndHeavy(ctx, ref, grown.ID, "synthetic terminated"); err != nil {
		t.Fatal(err)
	}
	base := p.Now()
	p.Now = func() time.Time { return base.Add(HostFreshness + time.Second) }
	if _, err := p.AcquireHeavy(ctx, ref, "stale-cmd", 1); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("a stale observation issued a permit: %v", err)
	}
}

func TestCRW1650HeavyPermitKeepsConfiguredCeiling(t *testing.T) {
	ctx := context.Background()
	p, ref := crw1650RunningHost(t)
	if _, err := p.AcquireHeavy(ctx, ref, "over-ceiling", 5); err == nil {
		t.Fatal("observed CPU alone widened the configured budget")
	}
	crw1650NoHeldPermits(t, p, ref.Executor.HostID)
}
