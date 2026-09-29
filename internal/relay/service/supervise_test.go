package service

import (
	"context"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

func testService(t *testing.T) *Service {
	t.Helper()
	home := t.TempDir()
	testsupport.Create(t, filepath.Join(home, "state/relay.sqlite3"), "", "go")
	s := &Service{Selection: store.StateSelection{Path: home + "/state"}, Scope: &ScopeRegistry{Root: home + "/scopes", Authority: "isolated"}, InstallationID: "test-installation"}
	if _, err := s.Enable("test"); err != nil {
		t.Fatal(err)
	}
	return s
}

// The supervisor opens its store in recovery (cli.py recover), under both service locks, and
// publishes the identity it read into its record and scope registration (publish_store_identity).
func Test29StoreOpensOnlyUnderBothLocks(t *testing.T) {
	s := testService(t)
	s.Socket = filepath.Join(t.TempDir(), "socket")
	opened := false
	var scopeWhileRunning Object
	recovery := func() error {
		opened = true
		if !existingLockHeld(s.path("daemon.lock")) || !existingLockHeld(s.Scope.path(s.Socket, ".lock")) {
			t.Fatal("writable store opened without both ownership locks")
		}
		if get(s.Record(), "storeId") != nil && get(s.Record(), "storeId") != s.StoreID {
			t.Fatalf("record names another store before recovery: %v", s.Record())
		}
		s.StoreID = "new-store"
		if err := s.PublishStoreIdentity(); err != nil {
			return err
		}
		scopeWhileRunning = s.Scope.Read(s.Socket)
		return nil
	}
	zero := 0
	if _, err := s.Supervise(context.Background(), Options{AllowIsolated: true, MaxSegments: &zero}, recovery, nil); err != nil {
		t.Fatal(err)
	}
	if !opened || get(s.Record(), "storeId") != "new-store" || get(scopeWhileRunning, "storeId") != "new-store" || get(s.Scope.Read(s.Socket), "storeId") != "new-store" {
		t.Fatalf("new identity not published: record %v, scope %v", s.Record(), s.Scope.Read(s.Socket))
	}
}

func Test29SupervisionBoundsAndRecoveryReadiness(t *testing.T) {
	for _, spent := range []bool{false, true} {
		t.Run(map[bool]string{false: "bounded", true: "spent"}[spent], func(t *testing.T) {
			s := testService(t)
			now := 100.0
			deadline := 100.35
			segment := .2
			recoveries := 0
			granted := []float64{}
			inputs := &SupervisionInputs{Now: func() float64 { return now }, Sleep: func(_ context.Context, n float64) error { now += n; return nil }, Spawn: func(lock, scope *os.File, token string, length float64, end *float64, allow bool) (*exec.Cmd, error) {
				if !truth(get(s.Record(), "readyAt")) {
					t.Fatal("worker before recovery readiness")
				}
				if end == nil || *end > deadline {
					t.Fatal("worker outlives supervisor")
				}
				if !existingLockHeld(filepath.Join(s.Selection.Path, "daemon.lock")) {
					t.Fatal("spawn without ownership")
				}
				granted = append(granted, *end)
				return &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}, nil
			}, Wait: func(*exec.Cmd) int { now += .2; return 0 }}
			if spent {
				now = 101
			}
			out, err := s.Supervise(context.Background(), Options{AllowIsolated: true, DeadlineMonotonic: &deadline, SegmentSeconds: &segment}, func() error {
				recoveries++
				if truth(get(s.Record(), "readyAt")) {
					t.Fatal("ready before recovery")
				}
				return nil
			}, inputs)
			if err != nil {
				t.Fatal(err)
			}
			if get(out, "ok") != true {
				t.Fatal(out)
			}
			if spent {
				if recoveries != 0 || len(granted) != 0 || get(s.Record(), "readyAt") != nil {
					t.Fatal(recoveries, granted, s.Record())
				}
			} else {
				if recoveries != 1 || len(granted) != 1 || granted[0] != 100.2 || math.Abs(now-deadline) > 1e-8 {
					t.Fatal(recoveries, granted, now)
				}
			}
		})
	}
}
func Test29SupervisionFailureBackoffAndIntent(t *testing.T) {
	s := testService(t)
	count := 4
	now := 0.0
	waits := []float64{}
	codes := []int{3, 3, 3, 0}
	i := 0
	out, err := s.Supervise(context.Background(), Options{AllowIsolated: true, MaxSegments: &count}, nil, &SupervisionInputs{Now: func() float64 { return now }, Sleep: func(_ context.Context, n float64) error { waits = append(waits, n); now += n; return nil }, Spawn: func(*os.File, *os.File, string, float64, *float64, bool) (*exec.Cmd, error) {
		return &exec.Cmd{Process: &os.Process{Pid: os.Getpid()}}, nil
	}, Wait: func(*exec.Cmd) int { code := codes[i]; i++; return code }})
	if err != nil {
		t.Fatal(err)
	}
	if len(waits) != 3 || waits[0] != 2 || waits[1] != 4 || waits[2] != 8 || get(out, "consecutiveFailures") != 0 || get(out, "degraded") != "3 consecutive worker failures, last exit 3" {
		t.Fatal(out, waits)
	}
	if RestartDelay(1000000) != 300 {
		t.Fatal("uncapped backoff")
	}
}
func Test29ScopePersistentConflictAndConcurrentReaders(t *testing.T) {
	s := testService(t)
	s.Socket = filepath.Join(t.TempDir(), "socket")
	s.StoreID = "first-store"
	claim, err := s.Scope.Claim(s.Socket, s.NewRecord(os.Getpid(), "test-run"))
	if err != nil || get(claim, "ok") != true {
		t.Fatal(claim, err)
	}
	if err = s.Scope.Release(s.Socket); err != nil {
		t.Fatal(err)
	}
	other := &ScopeRegistry{Root: s.Scope.Root, Authority: s.Scope.Authority}
	r := set(s.NewRecord(os.Getpid(), "next-run"), "storeId", "second-store")
	claim, err = other.Claim(s.Socket, r)
	if err != nil || get(claim, "reason") != "scope_registered_to_other_store" {
		t.Fatal(claim, err)
	}
	claim, err = other.Claim(s.Socket, set(r, "takeover", true))
	if err != nil || get(claim, "ok") != true {
		t.Fatal(claim, err)
	}
	defer other.Release(s.Socket)
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				if s.Scope.Read(s.Socket) == nil {
					t.Error("lost scope record")
				}
				_ = s.Scope.Conflicts(s.Socket, s.StoreID, s.Selection.Path)
				_ = s.Intent()
				_ = s.ResolveLaunchPolicy()
			}
		}()
	}
	wg.Wait()
}
