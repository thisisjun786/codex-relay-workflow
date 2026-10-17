package capacity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/crwconfig"
	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func hostPool(t *testing.T, sessions, slots, cpu int) (*HostPool, crwconfig.ExecutionHost) {
	t.Helper()
	s, err := store.Open(context.Background(), filepath.Join(t.TempDir(), "relay.sqlite3"), "synthetic-socket")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	p := &HostPool{Store: s, Now: func() time.Time { return time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC) }}
	h := crwconfig.ExecutionHost{ID: "host-a", Transport: "local", Root: filepath.Join(t.TempDir(), "worker"), Repositories: map[string]string{"owner/repo": "/synthetic/repo"}, LaunchProfile: strings.Repeat("c", 64), ToolchainProfile: strings.Repeat("d", 64), Kind: executor.Devin, CapabilityRevision: "cap-1", AuthProfile: "approved-auth", TrustProfile: "approved-trust", Supervision: "worker-rpc"}
	if err := p.Add(context.Background(), h); err != nil {
		t.Fatal(err)
	}
	check := HostObservation{HostID: h.ID, Incarnation: "inc-1", BootID: "boot-1", Sequence: 1, EffectiveCPU: 16, AvailableBytes: 32 << 30, SwapUsedPercent: 0, PressureSomeAvg60: 0, PressureFullAvg10: 0, Measured: true, ToolchainProfile: h.ToolchainProfile, LaunchProfile: h.LaunchProfile, CapabilityRevision: h.CapabilityRevision, Supervised: true}
	if err := p.Check(context.Background(), check); err != nil {
		t.Fatal(err)
	}
	if err := p.Enable(context.Background(), h.ID, crwconfig.HostCapacity{Sessions: sessions, TestSlots: slots, TestCPU: cpu}); err != nil {
		t.Fatal(err)
	}
	return p, h
}
func hostAdmission(t *testing.T, h crwconfig.ExecutionHost, id string) (executor.Admission, executor.Grant) {
	t.Helper()
	task := executor.Task{Schema: executor.TaskSchema, Identity: executor.Identity{TaskID: id, ExecutionID: id, Attempt: 1, AssignmentGeneration: 1, Plan: "plan-" + id, Node: "n", Revision: "1", CriteriaDigest: strings.Repeat("a", 64)}, Repository: executor.Repository{Forge: "github", ID: "owner/repo", BaseCommit: strings.Repeat("b", 40), TargetRef: "refs/heads/dev", TaskBranch: "refs/heads/task/" + id}, Regions: []executor.Region{{Repository: "owner/repo", Path: id + ".go", Kind: "file", Change: "edit"}}, WorkKind: "implementation", Executor: executor.Binding{HostID: h.ID, Incarnation: "inc-1", Kind: h.Kind, CapabilityRevision: h.CapabilityRevision, LaunchProfileDigest: h.LaunchProfile, ToolchainProfileDigest: h.ToolchainProfile}, Input: executor.Input{Kind: "prompt", Prompt: "work", BriefDigest: executor.Digest([]byte("work"))}, Requested: executor.Requested{Model: "synthetic", Effort: "high"}, Checks: []executor.Check{{ID: "check", Args: []string{"go", "test", "./..."}, Resources: executor.Resources{CPU: 1, Heavy: true}}}}
	a, err := executor.Freeze(task)
	if err != nil {
		t.Fatal(err)
	}
	return a, executor.Grant{ExecutionID: id, AssignmentGeneration: 1, Owner: "parent", OwnerEpoch: 1, ExpiresAt: time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)}
}

// The callback stands for the existing DAG/global reservation, on the same connection.
func reserveHost(p *HostPool, a executor.Admission, g executor.Grant) (Assignment, error) {
	return p.Assign(context.Background(), a, g, func(ctx context.Context) (string, error) {
		_, err := p.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO host_test_global(id) VALUES (?)", a.Task.Identity.ExecutionID)
		return a.Task.Identity.ExecutionID, err
	})
}
func globalTable(t *testing.T, p *HostPool) {
	t.Helper()
	if _, err := p.Store.DB.Exec("CREATE TABLE host_test_global(id TEXT PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
}
func TestHostLastSessionRaceRollsBackGlobalReservation(t *testing.T) {
	p, h := hostPool(t, 1, 1, 2)
	globalTable(t, p)
	peer, err := store.Open(context.Background(), p.Store.Path, "synthetic-socket")
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ps := []*HostPool{p, {Store: peer, Now: p.Now}}
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for i := range 2 {
		a, g := hostAdmission(t, h, fmt.Sprintf("e%d", i))
		wg.Add(1)
		go func(i int) { defer wg.Done(); _, err := reserveHost(ps[i], a, g); results <- err }(i)
	}
	wg.Wait()
	close(results)
	passed := 0
	for err := range results {
		if err == nil {
			passed++
		}
	}
	var count int
	if err := p.Store.DB.QueryRow("SELECT COUNT(*) FROM host_test_global").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if passed != 1 || count != 1 {
		t.Fatalf("admitted=%d global=%d", passed, count)
	}
}
func TestHostLastHeavySlotAndCPURace(t *testing.T) {
	for _, tc := range []struct {
		name       string
		slots, cpu int
	}{{"slot", 1, 8}, {"cpu", 2, 2}} {
		t.Run(tc.name, func(t *testing.T) {
			p, h := hostPool(t, 2, tc.slots, tc.cpu)
			globalTable(t, p)
			refs := []executor.Reference{}
			for i := range 2 {
				a, g := hostAdmission(t, h, fmt.Sprintf("e%d", i))
				if _, err := reserveHost(p, a, g); err != nil {
					t.Fatal(err)
				}
				refs = append(refs, executor.ReferenceOf(a))
				if err := p.ObserveExecution(context.Background(), executor.ReferenceOf(a), executor.Inspection{State: "running", NativeSession: executor.Observation{Value: "synthetic"}}); err != nil {
					t.Fatal(err)
				}
			}
			results := make(chan error, 2)
			for i := range 2 {
				go func(i int) {
					_, err := p.AcquireHeavy(context.Background(), refs[i], fmt.Sprintf("cmd-%d", i), 2)
					results <- err
				}(i)
			}
			passed := 0
			for range 2 {
				if <-results == nil {
					passed++
				}
			}
			if passed != 1 {
				t.Fatalf("admitted %d heavy groups", passed)
			}
		})
	}
}
func TestHostStaleAndRestartNeverReturnGrants(t *testing.T) {
	p, h := hostPool(t, 1, 1, 2)
	globalTable(t, p)
	a, g := hostAdmission(t, h, "e1")
	if _, err := reserveHost(p, a, g); err != nil {
		t.Fatal(err)
	}
	before := p.Now()
	p.Now = func() time.Time { return before.Add(91 * time.Second) }
	b, bg := hostAdmission(t, h, "e2")
	if _, err := reserveHost(p, b, bg); err == nil {
		t.Fatal("stale host admitted")
	}
	st, err := p.Status(context.Background(), h.ID)
	if err != nil || st.HeldSessions != 1 {
		t.Fatalf("stale lost grant: %+v %v", st, err)
	}
	obs := st.Observation
	obs.Incarnation = "inc-2"
	obs.Sequence = 1
	if err := p.Check(context.Background(), obs); err != nil {
		t.Fatal(err)
	}
	st, err = p.Status(context.Background(), h.ID)
	if err != nil || st.HeldSessions != 1 || st.Enabled {
		t.Fatalf("restart: %+v %v", st, err)
	}
	if err := p.ObserveExecution(context.Background(), executor.ReferenceOf(a), executor.Inspection{State: "unknown", NativeSession: executor.Observation{UnknownReason: "disconnected"}}); err != nil {
		t.Fatal(err)
	}
	st, _ = p.Status(context.Background(), h.ID)
	if st.HeldSessions != 1 {
		t.Fatal("unknown returned grant")
	}
}
func TestHostFailedAssignmentTransactionLeavesNothing(t *testing.T) {
	p, h := hostPool(t, 1, 1, 2)
	globalTable(t, p)
	a, g := hostAdmission(t, h, "e1")
	boom := errors.New("crash before commit")
	err := p.Store.Compose(context.Background(), func(ctx context.Context, _ *sql.Conn) error {
		_, err := p.Assign(ctx, a, g, func(ctx context.Context) (string, error) {
			_, err := p.Store.Querier(ctx).ExecContext(ctx, "INSERT INTO host_test_global VALUES ('e1')")
			return "e1", err
		})
		if err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatal(err)
	}
	st, err := p.Status(context.Background(), h.ID)
	if err != nil || st.HeldSessions != 0 {
		t.Fatalf("orphan: %+v %v", st, err)
	}
	var count int
	_ = p.Store.DB.QueryRow("SELECT COUNT(*) FROM host_test_global").Scan(&count)
	if count != 0 {
		t.Fatal("orphan global")
	}
}

func TestHostDispatchIntentCannotBeReissued(t *testing.T) {
	p, h := hostPool(t, 1, 1, 2)
	globalTable(t, p)
	a, g := hostAdmission(t, h, "e1")
	if _, err := reserveHost(p, a, g); err != nil {
		t.Fatal(err)
	}
	r := executor.ReferenceOf(a)
	first, err := p.ClaimStart(context.Background(), r)
	if err != nil || !first {
		t.Fatalf("claim: %v %v", first, err)
	}
	again, err := p.ClaimStart(context.Background(), r)
	if err != nil || again {
		t.Fatalf("duplicate send: %v %v", again, err)
	}
	if err = p.CancelIntent(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	again, err = p.ClaimStart(context.Background(), r)
	if err != nil || again {
		t.Fatal("lost cancellation allowed another start")
	}
	s, err := p.Status(context.Background(), h.ID)
	if err != nil || s.HeldSessions != 1 {
		t.Fatalf("grant: %+v %v", s, err)
	}
}
func TestHeavyPermitNeedsRunningGrant(t *testing.T) {
	p, h := hostPool(t, 1, 1, 2)
	globalTable(t, p)
	a, g := hostAdmission(t, h, "e1")
	if _, err := reserveHost(p, a, g); err != nil {
		t.Fatal(err)
	}
	if _, err := p.AcquireHeavy(context.Background(), executor.ReferenceOf(a), "test", 1); err == nil {
		t.Fatal("heavy command admitted before start")
	}
}
