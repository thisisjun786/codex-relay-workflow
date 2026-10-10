package recall

import (
	"errors"
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

// CRW-1154 d3 -- the retries of the hook's accounting stop with the budget; the first attempt always runs.
func TestHookCountingRetriesStopAtTheBudget(t *testing.T) {
	defer func(old time.Duration) { recallHookCountBudget = old }(recallHookCountBudget)
	for _, c := range []struct {
		budget time.Duration
		want   int
	}{{0, 1}, {100 * time.Millisecond, 2}, {time.Hour, 3}} {
		recallHookCountBudget = c.budget
		store := &slowFailingStore{delay: 60 * time.Millisecond}
		recallHookCountHits(RecallContextDeps{OpenHitCounts: func() (HitCountStore, error) { return store, nil }}, []string{"thread:x"})
		if store.bumps != c.want {
			t.Errorf("budget %v: %d attempts, want %d", c.budget, store.bumps, c.want)
		}
	}
}
