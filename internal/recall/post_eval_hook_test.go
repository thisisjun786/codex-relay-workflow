package recall

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type slowFailingStore struct {
	bumps int
	delay time.Duration
}

func (s *slowFailingStore) Read([]string) (map[string]float64, error) { return nil, nil }
func (s *slowFailingStore) Bump(string, []string) error {
	s.bumps++
	time.Sleep(s.delay)
	return errors.New("locked")
}
func (s *slowFailingStore) Close() error { return nil }

// CRW-1154 d3 -- the attempts of the hook's accounting stop with the budget; none starts once it is used up.
func TestHookCountingRetriesStopAtTheBudget(t *testing.T) {
	defer func(old time.Duration) { recallHookCountBudget = old }(recallHookCountBudget)
	for _, c := range []struct {
		budget time.Duration
		want   int
	}{{0, 0}, {100 * time.Millisecond, 2}, {time.Hour, 3}} {
		recallHookCountBudget = c.budget
		store := &slowFailingStore{delay: 60 * time.Millisecond}
		recallHookCountHits(RecallContextDeps{OpenHitCounts: func() (HitCountStore, error) { return store, nil }}, []string{"thread:x"}, time.Time{})
		if store.bumps != c.want {
			t.Errorf("budget %v: %d attempts, want %d", c.budget, store.bumps, c.want)
		}
	}
}

// CRW-1154 d3 -- the hook's own deadline bounds the accounting: with no time left no attempt starts, and the
// attempts stop at the deadline even when the counting budget is longer.
func TestHookCountingStopsAtTheHookDeadline(t *testing.T) {
	for _, c := range []struct {
		left time.Duration
		want int
	}{{-time.Second, 0}, {0, 0}, {100 * time.Millisecond, 2}} {
		store := &slowFailingStore{delay: 60 * time.Millisecond}
		recallHookCountHits(RecallContextDeps{OpenHitCounts: func() (HitCountStore, error) { return store, nil }}, []string{"thread:x"}, time.Now().Add(c.left))
		if store.bumps != c.want {
			t.Errorf("%v of the hook left: %d attempts, want %d", c.left, store.bumps, c.want)
		}
	}
}

// The deadline of the hook: its timeout from its start, the context's own deadline when earlier, less the reserve.
func TestRecallHookDeadline(t *testing.T) {
	defer func(a, b time.Duration) { recallHookTimeout, recallHookReserve = a, b }(recallHookTimeout, recallHookReserve)
	recallHookTimeout, recallHookReserve = 10*time.Second, 500*time.Millisecond
	started := time.Now()
	if got := recallHookDeadline(context.Background(), started); !got.Equal(started.Add(9500 * time.Millisecond)) {
		t.Errorf("no context deadline: %v", got.Sub(started))
	}
	ctx, cancel := context.WithDeadline(context.Background(), started.Add(3*time.Second))
	defer cancel()
	if got := recallHookDeadline(ctx, started); !got.Equal(started.Add(2500 * time.Millisecond)) {
		t.Errorf("earlier context deadline: %v", got.Sub(started))
	}
	ctx2, cancel2 := context.WithDeadline(context.Background(), started.Add(time.Hour))
	defer cancel2()
	if got := recallHookDeadline(ctx2, started); !got.Equal(started.Add(9500 * time.Millisecond)) {
		t.Errorf("later context deadline: %v", got.Sub(started))
	}
}

// heldLockHook runs the session-start hook against an index whose write lock another connection holds
// and whose rendering takes render of a hook that has timeout in all. It returns the elapsed time.
func heldLockHook(t *testing.T, timeout, reserve, render time.Duration) (time.Duration, *accountingHook, string) {
	t.Helper()
	oldTimeout, oldReserve := recallHookTimeout, recallHookReserve
	t.Cleanup(func() { recallHookTimeout, recallHookReserve = oldTimeout, oldReserve })
	recallHookTimeout, recallHookReserve = timeout, reserve
	h := newAccountingHook(t, 2)
	lock, err := openIndex(h.path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Exec("ROLLBACK"); _ = lock.Close() }()
	if err = lock.Exec("BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	started := time.Now()
	code := recallHookRun(context.Background(), "session-start", strings.NewReader(`{"cwd":"/repo","source":"startup"}`), &out, h.env, "/repo", func(_, _, _, _ string, deps RecallContextDeps) string {
		time.Sleep(render)
		deps.Rendered(h.refs)
		return `{"hookSpecificOutput":{"additionalContext":"valid context"}}`
	})
	elapsed := time.Since(started)
	if code != 0 || !strings.Contains(out.String(), "valid context") {
		t.Fatalf("exit %d, output %q", code, out.String())
	}
	return elapsed, h, out.String()
}

// CRW-1154 d3 (verification round 3) -- another process holds the index write lock while the rendering
// has used most of the hook's time: the hook ends inside its timeout, and counts nothing.
func TestHookWithAHeldWriteLockEndsInsideItsTimeout(t *testing.T) {
	const timeout = 2 * time.Second
	elapsed, h, _ := heldLockHook(t, timeout, 300*time.Millisecond, 1500*time.Millisecond)
	if elapsed >= timeout {
		t.Fatalf("the hook took %v of a %v timeout", elapsed, timeout)
	}
	for ref, n := range h.counts() {
		if n != 0 {
			t.Errorf("%s counted %v under a held lock", ref, n)
		}
	}
}

// With the rendering past the deadline no lock wait starts at all.
func TestHookWithNoTimeLeftDoesNotWaitForTheWriteLock(t *testing.T) {
	const render = 700 * time.Millisecond
	elapsed, _, _ := heldLockHook(t, 600*time.Millisecond, 100*time.Millisecond, render)
	if elapsed > render+250*time.Millisecond {
		t.Fatalf("the hook waited %v after a rendering of %v that already used its time", elapsed-render, render)
	}
}
