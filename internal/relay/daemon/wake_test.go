package daemon

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
)

// ticker is a tick that records when each tick began and what woke it.
type ticker struct {
	mu     sync.Mutex
	starts []time.Time
	woken  [][]string
	onTick func(n int)
}

func (k *ticker) tick(ctx context.Context) (Report, error) {
	k.mu.Lock()
	k.starts = append(k.starts, time.Now())
	k.woken = append(k.woken, Woken(ctx))
	n := len(k.starts)
	k.mu.Unlock()
	if k.onTick != nil {
		k.onTick(n)
	}
	return Report{}, nil
}

func (k *ticker) count() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.starts)
}

// A managed child's Stop wakes the daemon: the next tick runs once the debounce has passed and does not
// wait for the poll interval, which here is far longer than the test.
func TestWakeRunsTickWithoutPollInterval(t *testing.T) {
	t.Parallel()
	waker := NewWaker()
	k := &ticker{}
	k.onTick = func(n int) {
		if n == 1 {
			waker.Wake("child-1")
		}
	}
	two := 2
	began := time.Now()
	reports, err := RunWoken(t.Context(), k.tick, delivery.SystemClock{}, 30, &two, nil, nil, waker)
	took := time.Since(began)
	if err != nil || len(reports) != 2 {
		t.Fatalf("%d ticks, %v", len(reports), err)
	}
	if took > 10*time.Second {
		t.Fatalf("the woken tick waited %v for a 30 s poll interval", took)
	}
	if got := k.woken[1]; !slices.Equal(got, []string{"child-1"}) {
		t.Fatalf("the woken tick was told %v, want [child-1]", got)
	}
	if len(k.woken[0]) != 0 {
		t.Fatalf("the first tick was not woken but was told %v", k.woken[0])
	}
}

// A storm of wakes is one tick per debounce gap: the ticks begin at least MinWakeGap apart, however many
// wakes arrive, and the threads that woke a tick are listed once.
func TestWakeStormDebounced(t *testing.T) {
	t.Parallel()
	waker := NewWaker()
	k := &ticker{}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	var senders sync.WaitGroup
	senders.Add(1)
	go func() {
		defer senders.Done()
		for i := 0; ctx.Err() == nil; i++ {
			waker.Wake(fmt.Sprintf("child-%d", i%5))
			time.Sleep(time.Millisecond)
		}
	}()
	began := time.Now()
	stop := func() bool { return time.Since(began) > 2600*time.Millisecond }
	forever := 1000
	reports, err := RunWoken(ctx, k.tick, delivery.SystemClock{}, 30, &forever, nil, stop, waker)
	cancel()
	senders.Wait()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(reports); n < 2 || n > 4 {
		t.Fatalf("%d ticks in 2.6 s of constant wakes, want one per second (2 to 4)", n)
	}
	for i := 1; i < len(k.starts); i++ {
		if gap := k.starts[i].Sub(k.starts[i-1]); gap < MinWakeGap-50*time.Millisecond {
			t.Fatalf("ticks %d and %d began %v apart, under the %v floor", i-1, i, gap, MinWakeGap)
		}
	}
	for i, woken := range k.woken {
		if len(woken) > 5 || len(slices.Compact(slices.Sorted(slices.Values(woken)))) != len(woken) {
			t.Fatalf("tick %d was told %v: want each of the 5 threads at most once", i, woken)
		}
	}
}

// A woken tick observes the woken thread's turn before the others: the host is read for it first.
func TestWokenThreadObservedFirst(t *testing.T) {
	t.Parallel()
	read := func(woken []string) []string {
		ctx := t.Context()
		s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		for i := 1; i <= 3; i++ {
			seed(t, s, fmt.Sprintf("r%d", i), "parent", fmt.Sprintf("child-%d", i), fmt.Sprintf("anchor-%d", i))
		}
		host := &observationHost{status: "completed"}
		d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
		if _, err = d.Tick(WithWoken(ctx, woken)); err != nil {
			t.Fatal(err)
		}
		return host.reads
	}
	if got := read(nil); !slices.Equal(got, []string{"anchor-1", "anchor-2", "anchor-3"}) {
		t.Fatalf("without a wake the turns are read %v, want the relationships' order", got)
	}
	if got := read([]string{"child-3"}); len(got) != 3 || got[0] != "anchor-3" {
		t.Fatalf("woken child-3 was not read first: %v", got)
	}
	if got := read([]string{"child-2", "child-3"}); len(got) != 3 || !slices.Equal(got[:2], []string{"anchor-2", "anchor-3"}) {
		t.Fatalf("woken child-2 and child-3 were not read first, in the relationships' order: %v", got)
	}
	if got := read([]string{"unknown-thread"}); !slices.Equal(got, []string{"anchor-1", "anchor-2", "anchor-3"}) {
		t.Fatalf("a woken thread with nothing to observe changed the order: %v", got)
	}
}

// With nobody waking it the loop still ticks every poll interval.
func TestTickBackstopStillRuns(t *testing.T) {
	t.Parallel()
	waker := NewWaker()
	k := &ticker{}
	three := 3
	began := time.Now()
	reports, err := RunWoken(t.Context(), k.tick, delivery.SystemClock{}, 0.3, &three, nil, nil, waker)
	took := time.Since(began)
	if err != nil || len(reports) != 3 {
		t.Fatalf("%d ticks, %v", len(reports), err)
	}
	if took < 500*time.Millisecond || took > 10*time.Second {
		t.Fatalf("three ticks at a 0.3 s interval took %v", took)
	}
	for i, woken := range k.woken {
		if len(woken) != 0 {
			t.Fatalf("backstop tick %d was told it was woken by %v", i, woken)
		}
	}
}

// liveParent binds task as the live parent of project in the store (the orchestrator of the project).
func liveParent(t *testing.T, d *Daemon, task, project, status string) {
	t.Helper()
	if _, err := d.Store.DB.Exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES (?,?,?,?,?,?,?,1,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')",
		"b-"+task+"-"+project, "parent", "project", project, task, "host", status); err != nil {
		t.Fatal(err)
	}
}

// The tick's budgets are computed from the live orchestrators: none gives today's constants, and each
// live orchestrator brings its share, up to a bound.
func TestTickBudgetsScaleWithOrchestrators(t *testing.T) {
	t.Parallel()
	p := DefaultPolicy()
	share := delivery.DefaultPolicy().MaxSendsPerParentPerTick
	for _, c := range []struct {
		live int
		want Budgets
	}{
		{0, Budgets{MaxSends: 4, MaxProjects: 4, MaxSupervisorSends: 2, MaxSendsPerParentPerTick: 2}},
		{1, Budgets{MaxSends: 4, MaxProjects: 4, MaxSupervisorSends: 2, MaxSendsPerParentPerTick: 3}},
		{3, Budgets{MaxSends: 9, MaxProjects: 4, MaxSupervisorSends: 3, MaxSendsPerParentPerTick: 3}},
		{10, Budgets{MaxSends: 30, MaxProjects: 10, MaxSupervisorSends: 10, MaxSendsPerParentPerTick: 3}},
		{1000, Budgets{MaxSends: 96, MaxProjects: 32, MaxSupervisorSends: 32, MaxSendsPerParentPerTick: 3}},
		{-1, Budgets{MaxSends: 4, MaxProjects: 4, MaxSupervisorSends: 2, MaxSendsPerParentPerTick: 2}},
	} {
		if got := p.BudgetsFor(c.live, share); got != c.want {
			t.Errorf("%d live orchestrators: budgets %+v, want %+v", c.live, got, c.want)
		}
	}

	// The tick counts the live orchestrators of its store: an active and a paused parent binding of a project
	// (a task that holds two projects once), not a superseded one, a supervisor or a parent of an initiative.
	ctx := t.Context()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	d := New(s, &observationHost{status: "completed"}, &delivery.FakeClock{T: 1700000000}, nil)
	if _, err = d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if want := p.BudgetsFor(0, share); d.budgets != want {
		t.Fatalf("a store with no orchestrator ran on %+v, want %+v", d.budgets, want)
	}
	liveParent(t, d, "orch-a", "PROJ-1", "active")
	liveParent(t, d, "orch-a", "PROJ-2", "active")
	liveParent(t, d, "orch-b", "PROJ-3", "paused")
	liveParent(t, d, "orch-c", "PROJ-4", "superseded")
	if _, err = s.DB.Exec("INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, status, revision, created_at, updated_at) VALUES ('b-s','supervisor','initiative','INIT-1','lead','host','active',1,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z'), ('b-p','parent','initiative','INIT-2','orch-d','host','active',1,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')"); err != nil {
		t.Fatal(err)
	}
	if _, err = d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if want := p.BudgetsFor(2, share); d.budgets != want || d.budgets.MaxSends != 6 {
		t.Fatalf("a store with two live orchestrators ran on %+v, want %+v", d.budgets, want)
	}
}
