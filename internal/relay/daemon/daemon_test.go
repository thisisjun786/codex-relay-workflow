package daemon

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
	"golang.org/x/sys/unix"
)

func TestMain(m *testing.M) { testsupport.Main(m) }
func Test29InheritedDescriptionSurvivesClose(t *testing.T) {
	directory := t.TempDir()
	parent, err := Acquire(directory, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	fd, err := unix.Dup(int(parent.File.Fd()))
	if err != nil {
		t.Fatal(err)
	}
	worker, err := Acquire(directory, false, &fd)
	if err != nil {
		t.Fatal(err)
	}
	defer worker.Close()
	if err = parent.Close(); err != nil {
		t.Fatal(err)
	}
	if next, err := Acquire(directory, false, nil); err == nil {
		next.Close()
		t.Fatal("closing supervisor unlocked worker")
	}
	if err = worker.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Acquire(directory, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = next.Close(); err != nil {
		t.Fatal(err)
	}
}
func Test29CadenceBounds(t *testing.T) {
	ctx := context.Background()
	clock := &delivery.FakeClock{T: 10}
	ticks := 0
	tick := func(context.Context) (Report, error) { ticks++; return Report{}, nil }
	count := 3
	waits := []float64{}
	sleep := func(n float64) error { waits = append(waits, n); clock.T += n; return nil }
	reports, err := Run(ctx, tick, clock, 20, &count, nil, nil, sleep)
	if err != nil || len(reports) != 3 || ticks != 3 || len(waits) != 2 || clock.T != 50 {
		t.Fatalf("%v ticks=%d waits=%v clock=%v", err, ticks, waits, clock.T)
	}
	if _, err = Run(ctx, tick, clock, 20, nil, nil, func() bool { return false }, nil); err == nil {
		t.Fatal("stop alone became a bound")
	}
	deadline := clock.T
	reports, err = Run(ctx, tick, clock, 20, nil, &deadline, nil, sleep)
	if err != nil || len(reports) != 0 {
		t.Fatal(reports, err)
	}
	count = 0
	reports, err = Run(ctx, tick, clock, 20, &count, nil, nil, sleep)
	if err != nil || len(reports) != 0 {
		t.Fatal(reports, err)
	}
	count = 4
	reports, err = Run(ctx, tick, clock, 20, &count, nil, func() bool { return true }, sleep)
	if err != nil || len(reports) != 0 {
		t.Fatal(reports, err)
	}
}

// fixtureStore opens a fresh store: Open creates an absent one as owner=go at epoch 1.
func fixtureStore(ctx context.Context, path, socket string) (*store.Store, error) {
	return store.Open(ctx, path, socket)
}
func Test29QuietConcurrentTicks(t *testing.T) {
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := New(s, nil, &delivery.FakeClock{T: 1700000000}, nil)
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := d.Tick(ctx)
			if err != nil {
				t.Error(err)
			}
			if !r.Quiet() || len(r.Notes) != 0 {
				t.Errorf("quiet tick %+v", r)
			}
		}()
	}
	wg.Wait()
	var n int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM journal").Scan(&n); err != nil || n != 0 {
		t.Fatal(n, err)
	}
}

type observationHost struct {
	delivery.Adapter
	status string
	reads  []string
	onRead func()
}

func (h *observationHost) Close() error { return nil }
func (h *observationHost) ReadTurn(thread, turn string) (*delivery.TurnInfo, error) {
	h.reads = append(h.reads, turn)
	if h.onRead != nil {
		h.onRead()
	}
	if h.status == "absent" {
		return nil, nil
	}
	return &delivery.TurnInfo{TurnID: turn, Status: h.status}, nil
}
func seed(t *testing.T, s *store.Store, rid, parent, child, turn string) {
	t.Helper()
	_, err := s.DB.Exec("INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES(?,?,'active',?,'host','/parent',?,'host','/child',1,'[]',?, '2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')", rid, "REL-1", parent, child, `["`+parent+`"]`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.DB.Exec("INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES(?,1,?,'bound',?,'initial_assignment','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')", rid, "dispatch-"+rid, turn)
	if err != nil {
		t.Fatal(err)
	}
}
func Test29ObservationIsPerAssignment(t *testing.T) {
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seed(t, s, "r1", "parent-one", "shared-child", "anchor")
	seed(t, s, "r2", "parent-two", "shared-child", "anchor")
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	r, err := d.Tick(ctx)
	if err != nil || r.Observed != 2 {
		t.Fatalf("%+v %v", r, err)
	}
	var n int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM assignment_settlements").Scan(&n); err != nil || n != 2 {
		t.Fatal(n, err)
	}
	r, err = d.Tick(ctx)
	if err != nil || !r.Quiet() || len(host.reads) != 2 {
		t.Fatalf("%+v reads=%v err=%v", r, host.reads, err)
	}
}
func Test29AbsentAnchorDoesNotClaimHealthyPoll(t *testing.T) {
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seed(t, s, "r", "parent", "child", "anchor")
	d := New(s, &observationHost{status: "absent"}, &delivery.FakeClock{T: 1700000000}, nil)
	if _, err = d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	row, err := s.One(ctx, "SELECT last_polled_at,last_error FROM poll_observations")
	if err != nil || row.Get("last_polled_at") != nil || row.Get("last_error") != "str: the host reports this turn absent" {
		t.Fatal(row, err)
	}
}
func Test29AdmissionRotationDoesNotSkipUnservedTurns(t *testing.T) {
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	seed(t, s, "r", "parent", "child", "anchor")
	for _, turn := range []string{"one", "two", "three"} {
		_, err = s.DB.Exec("INSERT INTO generation_turns(relationship_id,execution_generation,turn_id,evidence,actor,detail,admitted_at) VALUES('r',1,?,'explicit_admission_bound:anchor','child','continuation','2023-11-14T22:13:20Z')", turn)
		if err != nil {
			t.Fatal(err)
		}
	}
	host := &observationHost{status: "completed"}
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxTurnReads = 1
	for range 8 {
		if _, err = d.Tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if len(host.reads) != 4 {
		t.Fatalf("starved admissions: %v", host.reads)
	}
}
