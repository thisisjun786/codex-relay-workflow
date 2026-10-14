package engine

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// CRW-1348 (CRW-1316 §10): the run wakes its owner thread. A step published while the owner is idle or notLoaded stages a wake;
// while no subagent is dispatched the run notifies again at growing intervals up to RenotifyMax; after UnresponsiveAfter it writes
// one session_unresponsive escalation row and goes on; once the step is dispatched the wakes stop. The logical id of a wake is
// (run, batch).

type wakeRecorder struct {
	mu    sync.Mutex
	wakes []SessionWake
	at    []time.Time
}

func (r *wakeRecorder) wake(_ context.Context, w SessionWake) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.wakes = append(r.wakes, w)
	r.at = append(r.at, time.Now())
	return nil
}

func (r *wakeRecorder) got() ([]SessionWake, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]SessionWake(nil), r.wakes...), append([]time.Time(nil), r.at...)
}

func wakeKit(t *testing.T, ownerStatus string) (*harness, *fakeOrch, *wakeRecorder) {
	t.Helper()
	h := newHarness(t)
	host, placer, bus := newFakeSessionHost(), newSessPlacer(), NewFileStepBus(t.TempDir())
	host.agents[sessionOwner] = &fakeAgent{SessionAgent: SessionAgent{ThreadID: sessionOwner, Status: ownerStatus}}
	rec := &wakeRecorder{}
	h.cfg.Backend = placer
	h.cfg.Session = &SessionConfig{Host: host, Bus: bus, Owner: sessionOwner, Poll: 2 * time.Millisecond, LostAfter: time.Minute,
		WakeAfter: 40 * time.Millisecond, RenotifyMax: 80 * time.Millisecond, UnresponsiveAfter: 300 * time.Millisecond, Wake: rec.wake}
	return h, newFakeOrch(bus, host, placer), rec
}

func escalations(j Journal, reason string) []Event {
	var out []Event
	for _, e := range flowRows(j, EvEscalation) {
		if rowStr(e.Data, "reason") == reason {
			out = append(out, e)
		}
	}
	return out
}

func TestSessionWakesTheOwnerAndEscalatesWithoutStopping(t *testing.T) {
	h, orch, rec := wakeKit(t, "idle")
	body := `const a = await agent.call('look', { agentType: 'explorer', label: 'a' })
return a.status`
	done := make(chan *Result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, err := Run(ctx, h.load(body), h.cfg)
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()
	deadline := time.Now().Add(10 * time.Second)
	for len(escalations(h.journal, ReasonSessionUnresponsive)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no session_unresponsive escalation")
		}
		time.Sleep(5 * time.Millisecond)
	}
	// the run did not stop: the owner takes the step now and the call completes
	orch.start(t)
	res := <-done
	if got := value[string](t, res); got != StatusCompleted {
		t.Fatalf("status %q", got)
	}
	wakes, at := rec.got()
	if len(wakes) < 4 {
		t.Fatalf("%d wakes, want the first and at least three re-notifications", len(wakes))
	}
	first := wakes[0]
	if first.Renotify != 0 || first.ID != first.Batch || !strings.HasPrefix(first.Batch, "b") || len(first.Ops) != 1 || first.Run != "run-test" ||
		first.Owner != sessionOwner || first.OwnerStatus != "idle" {
		t.Fatalf("the first wake %+v", first)
	}
	for i, w := range wakes[1:] {
		if w.Batch != first.Batch || w.Renotify != i+1 || w.ID != first.Batch+"/r"+strconv.Itoa(i+1) {
			t.Fatalf("re-notification %d: %+v", i+1, w)
		}
	}
	// the intervals grow and stop growing at RenotifyMax
	if d := at[1].Sub(at[0]); d < 35*time.Millisecond {
		t.Errorf("the first re-notification came after %s, want WakeAfter", d)
	}
	if d1, d2 := at[2].Sub(at[1]), at[1].Sub(at[0]); d1 < d2 {
		t.Errorf("the interval shrank: %s after %s", d1, d2)
	}
	esc := escalations(h.journal, ReasonSessionUnresponsive)
	if len(esc) != 1 || rowStr(esc[0].Data, "owner") != sessionOwner || rowStr(esc[0].Data, "batch") != first.Batch {
		t.Fatalf("escalations %+v", esc)
	}
	staged := flowRows(h.journal, EvSessionWake)
	if len(staged) != len(wakes) || staged[0].Data["staged"] != true || rowStr(staged[0].Data, "wake_id") != first.ID {
		t.Fatalf("session_wake rows %+v", staged)
	}
	// once dispatched, the step wakes nobody
	n := len(wakes)
	time.Sleep(150 * time.Millisecond)
	if after, _ := rec.got(); len(after) != n {
		t.Fatalf("%d wakes after the dispatch", len(after)-n)
	}
}

func TestSessionDoesNotWakeABusyOwnerFirst(t *testing.T) {
	h, orch, rec := wakeKit(t, "active")
	h.cfg.Session.UnresponsiveAfter = time.Hour
	body := `const a = await agent.call('look', { agentType: 'explorer', label: 'a' })
return a.status`
	done := make(chan *Result, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		res, _ := Run(ctx, h.load(body), h.cfg)
		done <- res
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if wakes, _ := rec.got(); len(wakes) > 0 {
			if wakes[0].Renotify != 1 || wakes[0].OwnerStatus != "active" {
				t.Fatalf("a busy owner's first wake %+v", wakes[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no re-notification of a busy owner")
		}
		time.Sleep(5 * time.Millisecond)
	}
	orch.start(t)
	if res := <-done; res == nil || value[string](t, res) != StatusCompleted {
		t.Fatalf("result %+v", res)
	}
}

func TestSessionRenotifyScheduleGrowsToTheMax(t *testing.T) {
	s := &SessionConfig{WakeAfter: 15 * time.Minute, RenotifyMax: time.Hour}
	var got []string
	for n := 1; n <= 5; n++ {
		got = append(got, s.renotifyDue(n).String())
	}
	if strings.Join(got, ",") != "15m0s,45m0s,1h45m0s,2h45m0s,3h45m0s" {
		t.Fatalf("schedule %v", got)
	}
	d := &SessionConfig{}
	if d.wakeAfter() != 15*time.Minute || d.renotifyMax() != time.Hour || d.unresponsiveAfter() != 2*time.Hour {
		t.Fatal("the defaults")
	}
}
