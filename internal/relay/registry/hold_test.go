package registry

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// test_registration_hold.py and test_registration_contention.py, the relay half: a marker
// registration decides its dispatch's generation under the store's write lock.

func registeredForHold(t *testing.T) (*Registry, string) {
	t.Helper()
	r := newRegistry(t)
	x, err := r.Register(ctx(), fixture())
	if err != nil {
		t.Fatal(err)
	}
	return r, x.ID
}

func second(t *testing.T, r *Registry) *Registry {
	t.Helper()
	s, err := store.Open(ctx(), r.Store.Path, "")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return &Registry{Store: s, Now: r.Now, Policy: r.Policy}
}

// advanceHeld opens generation 2 on another store and sits inside the uncommitted
// transaction until release is closed.
func advanceHeld(t *testing.T, r *Registry, rid string, holding chan<- struct{}, release <-chan struct{}) <-chan error {
	t.Helper()
	other := second(t, r)
	done := make(chan error, 1)
	go func() {
		done <- other.Store.Transaction(ctx(), func(ctx context.Context, _ *sql.Conn) error {
			if _, err := other.OpenGenerationIn(ctx, rid, "revision-held", "needs_changes_revision", ns("turn-held")); err != nil {
				return err
			}
			close(holding)
			<-release
			return nil
		})
	}()
	return done
}

func generationNow(t *testing.T, r *Registry, rid string) int64 {
	t.Helper()
	x, err := r.Get(ctx(), rid)
	if err != nil {
		t.Fatal(err)
	}
	return x.Generation
}

// RHD-1: while an advance holds the store's write lock, a registration cannot publish: it is
// refused unregistered_relationship naming the lock, nothing is published, and after the advance
// commits the dispatch reads stale.
func Test25_RHD1_a_registration_cannot_publish_while_an_advance_holds_the_store(t *testing.T) {
	r, rid := registeredForHold(t)
	holding, release := make(chan struct{}), make(chan struct{})
	done := advanceHeld(t, r, rid, holding, release)
	select {
	case <-holding:
	case <-time.After(20 * time.Second):
		t.Fatal("the advance never opened its transaction")
	}
	published := false
	err := RegisterUnderHold(ctx(), r.Store.Path, rid, "dispatch-1", func(int64) error { published = true; return nil })
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("the advance failed: %v", err)
	}
	mustReason(t, err, contract.RefusalUnregisteredRelationship)
	if !strings.Contains(err.Error(), "the relay store's write lock could not be taken: database is locked") {
		t.Fatalf("the refusal blamed something other than the lock: %v", err)
	}
	if published {
		t.Fatal("published while an advance held the store")
	}
	if g := generationNow(t, r, rid); g != 2 {
		t.Fatalf("generation %d", g)
	}
	state, readable := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1")
	if !readable || state != DispatchStale {
		t.Fatal(state, readable)
	}
}

// RHD-2: the hold covers the publication: an advance attempted during publish cannot take the
// lock (observed with a zero busy timeout, so no clock is involved); once publish returns the same
// advance commits, the publication named generation 1 and the dispatch then reads stale.
func Test25_RHD2_an_advance_cannot_commit_while_a_registration_is_publishing(t *testing.T) {
	r, rid := registeredForHold(t)
	impatient, err := store.OpenWith(ctx(), r.Store.Path, "", store.OpenOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = impatient.Close() })
	other := &Registry{Store: impatient, Now: r.Now, Policy: r.Policy}
	var named int64
	var during error
	err = RegisterUnderHold(ctx(), r.Store.Path, rid, "dispatch-1", func(generation int64) error {
		named = generation
		_, during = other.OpenGeneration(ctx(), rid, "revision-held", "needs_changes_revision", ns("turn-held"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if during == nil || !strings.Contains(during.Error(), "locked") {
		t.Fatalf("an advance was not held off during the publication: %v", during)
	}
	if named != 1 {
		t.Fatalf("published generation %d", named)
	}
	if _, err := other.OpenGeneration(ctx(), rid, "revision-held", "needs_changes_revision", ns("turn-held")); err != nil {
		t.Fatalf("advance after the hold: %v", err)
	}
	if state, _ := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1"); state != DispatchStale {
		t.Fatal(state)
	}
}

// RCT-3: after an advance, dispatch_generation_state says stale, not absent; an unknown
// dispatch is absent; an unopenable store is (absent, unreadable) and is not created.
func Test25_RCT3_a_dispatch_after_an_advance_reads_stale_not_absent(t *testing.T) {
	r, rid := registeredForHold(t)
	if state, readable := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1"); state != DispatchCurrent || !readable {
		t.Fatal(state)
	}
	if _, err := r.OpenGeneration(ctx(), rid, "revision-after", "needs_changes_revision", ns("turn-after")); err != nil {
		t.Fatal(err)
	}
	if state, readable := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1"); state != DispatchStale || !readable {
		t.Fatal(state, readable)
	}
	if state, _ := DispatchGenerationState(ctx(), r.Store.Path, rid, "never"); state != DispatchAbsent {
		t.Fatal(state)
	}
	missing := t.TempDir() + "/absent/relay.sqlite3"
	if state, readable := DispatchGenerationState(ctx(), missing, rid, "dispatch-1"); state != DispatchAbsent || readable {
		t.Fatal(state, readable)
	}
}

// RCT-2: a registration racing an advance either publishes naming a generation that was current
// when it landed, or is refused stale_generation, never both; the store stays readable.
func Test25_RCT2_a_registration_racing_an_advance_publishes_or_is_refused_never_both(t *testing.T) {
	for round := 0; round < 10; round++ {
		r, rid := registeredForHold(t)
		other := second(t, r)
		var wg sync.WaitGroup
		start := make(chan struct{})
		var regErr, advErr error
		var published []int64
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			for {
				regErr = RegisterUnderHold(ctx(), r.Store.Path, rid, "dispatch-1", func(g int64) error {
					published = append(published, g)
					return nil
				})
				// A hold that could not be taken within the timeout is not the race's answer; retry.
				if regErr == nil || !strings.Contains(regErr.Error(), "write lock could not be taken") {
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			<-start
			_, advErr = other.OpenGeneration(ctx(), rid, "revision-2", "needs_changes_revision", ns("turn-2"))
		}()
		close(start)
		wg.Wait()
		if advErr != nil {
			t.Fatal(advErr)
		}
		switch {
		case regErr == nil:
			if len(published) != 1 || published[0] != 1 {
				t.Fatalf("published %v", published)
			}
		default:
			mustReason(t, regErr, contract.RefusalStaleGeneration)
			if len(published) != 0 {
				t.Fatal("refused and published")
			}
		}
		if _, readable := DispatchGenerationState(ctx(), r.Store.Path, rid, "dispatch-1"); !readable {
			t.Fatal("store became unreadable")
		}
	}
}
