package capacity

import (
	"context"
	"errors"
	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor/worker"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestToolchainApplyRequiresDrainAndRetainsLostReadback(t *testing.T) {
	p, h := hostPool(t, 1, 1, 2)
	source := filepath.Join(t.TempDir(), "tool")
	if err := os.WriteFile(source, []byte("synthetic"), 0600); err != nil {
		t.Fatal(err)
	}
	m := worker.Toolchain{OS: runtime.GOOS, Arch: runtime.GOARCH, Files: []worker.ToolFile{{Path: "bin/tool", Version: "1", Source: source, SHA256: executor.Digest([]byte("synthetic"))}}}
	plan, err := p.PlanTools(context.Background(), h.ID, m)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	apply := func(context.Context, ToolPlan) (worker.ToolchainReadback, error) {
		calls++
		return worker.InstallToolchain(h.Root, m)
	}
	if _, err = p.ApplyTools(context.Background(), plan.ID, apply); err == nil || calls != 0 {
		t.Fatal("applied on enabled host")
	}
	if err = p.Drain(context.Background(), h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = p.ApplyTools(context.Background(), plan.ID, func(ctx context.Context, p ToolPlan) (worker.ToolchainReadback, error) {
		_, err := apply(ctx, p)
		if err != nil {
			return worker.ToolchainReadback{}, err
		}
		return worker.ToolchainReadback{}, errors.New("reply lost")
	}); err == nil {
		t.Fatal("lost reply passed")
	}
	if err = p.Enable(context.Background(), h.ID, crwconfig.HostCapacity{Sessions: 1, TestSlots: 1, TestCPU: 2}); err == nil {
		t.Fatal("enabled while application unknown")
	}
	done, err := p.ApplyTools(context.Background(), plan.ID, apply)
	if err != nil || done.State != "applied" {
		t.Fatalf("%+v %v", done, err)
	}
	s, _ := p.Status(context.Background(), h.ID)
	if s.Enabled || s.Checked || s.Host.ToolchainProfile != m.Digest() {
		t.Fatalf("apply needs fresh check: %+v", s)
	}
}
