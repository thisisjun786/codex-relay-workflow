package daemon

import (
	"context"
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1007 (decisions 2 and 3): the idle pass's two store sites go through Daemon.halted, as the other
// call sites in daemon.go do. The damage is a real SQLite corruption (code 11, the class CRW-848's tests
// use), so the classifier in store/halt.go decides whether a failure halts. A store that is damaged is
// not queried afterwards, so the assertions read the marker file, the process's halt and the tick report.

// idleWakeCorruptStore makes the store's next read of a page fail as SQLITE_CORRUPT: the page cache is
// dropped and every page after the first is overwritten on disk.
func idleWakeCorruptStore(t *testing.T, s *store.Store) {
	t.Helper()
	exec(t, s, "PRAGMA wal_checkpoint(TRUNCATE)")
	exec(t, s, "PRAGMA shrink_memory")
	damageStorePages(t, s.Path)
}

// TestIdleWakeHalt_a_failed_wake_write_ends_the_tick_before_delivery: the wake is the idle pass's own write.
// Its corruption publishes the write marker, keeps the halt in the process, ends the tick without an error,
// and the delivery pass that would attempt the waiting head never runs.
func TestIdleWakeHalt_a_failed_wake_write_ends_the_tick_before_delivery(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}}
	d, s := idleDaemon(t, host)
	d.idle.beforeIdle = func() { idleWakeCorruptStore(t, s) }
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting wake write returned %v instead of halting the store", err)
	}
	if r.Delivered != 0 || r.Deferred != 0 {
		t.Fatalf("the delivery pass ran after the failed wake write: delivered %d, deferred %d", r.Delivered, r.Deferred)
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteWrite)
	idleWakeAssertNextTickWritesNothing(t, d, host)
}

// TestIdleWakeHalt_a_failed_head_read_ends_the_tick: the waiting heads are read out of the store after the
// delivery pass. A corrupting read publishes the observation marker, keeps the halt in the process, ends the
// tick, and opens no subscription on a store that could not be read.
func TestIdleWakeHalt_a_failed_head_read_ends_the_tick(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s := idleDaemon(t, host)
	d.idle.beforeHold = func() { idleWakeCorruptStore(t, s) }
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("a corrupting head read returned %v instead of halting the store", err)
	}
	if len(host.holds) != 0 {
		t.Fatalf("a subscription was opened on a store that could not be read: %v", host.holds)
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteObservation)
	idleWakeAssertNextTickWritesNothing(t, d, host)
}

// TestIdleWakeHalt_a_failed_head_read_keeps_the_delivery_counts: the delivery pass ran before the failed
// read, so its deferral is part of the halted tick's report and is not dropped with the halt.
func TestIdleWakeHalt_a_failed_head_read_keeps_the_delivery_counts(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s := idleDaemon(t, host)
	// The head is due and its recipient is busy, so the delivery pass defers it before the read fails.
	exec(t, s, "UPDATE deliveries SET next_eligible_at = ? WHERE event_id = ?", idleNow-1, idleEvent)
	d.idle.beforeHold = func() { idleWakeCorruptStore(t, s) }
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting head read returned %v instead of halting the store", err)
	}
	if r.Deferred != 1 {
		t.Fatalf("the delivery pass's deferral is missing from the halted tick: deferred %d", r.Deferred)
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteObservation)
}

// TestIdleWakeHalt_a_failure_that_is_not_corruption_stays_the_tick_error: a failure the classifier does not
// call corruption is left to the caller exactly as before: the tick returns it and publishes no marker.
func TestIdleWakeHalt_a_failure_that_is_not_corruption_stays_the_tick_error(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}}
	d, s := idleDaemon(t, host)
	exec(t, s, "DROP TABLE delivery_wakes")
	if _, err := d.Tick(context.Background()); err == nil {
		t.Fatal("a failure that is not corruption returned no error")
	}
	if state := store.HaltStateAt(s.Path); state.Present {
		t.Fatalf("a failure that is not corruption published a halt marker: %+v", state.Marker)
	}
	if d.haltedStore {
		t.Fatal("a failure that is not corruption set the in-process halt")
	}
}

// idleWakeAssertHaltMarker checks what a corrupting failure leaves: the marker on disk with the corrupting
// class (code 11) at the site the pass names, and the in-process halt the next pass reads first.
func idleWakeAssertHaltMarker(t *testing.T, d *Daemon, site string) {
	t.Helper()
	state := store.HaltStateAt(d.Store.Path)
	if !state.Present || state.Detail != "" {
		t.Fatalf("no corruption marker was published: %+v", state)
	}
	if state.Marker.Code != 11 || state.Marker.Site != site {
		t.Fatalf("marker %+v, want code 11 at site %s", state.Marker, site)
	}
	if !d.haltedStore {
		t.Fatal("the marker was published but the process did not keep its halt")
	}
}

// idleWakeAssertNextTickWritesNothing: with the disk marker removed, the process's own halt still makes the
// next tick answer from its reason alone. It attempts no host read and publishes no marker again.
func idleWakeAssertNextTickWritesNothing(t *testing.T, d *Daemon, host *idleHost) {
	t.Helper()
	state := store.HaltStateAt(d.Store.Path)
	if err := os.Remove(state.Path); err != nil {
		t.Fatal(err)
	}
	holds := host.holdTries
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("the tick after the halt errored: %v", err)
	}
	reported := false
	for _, note := range r.Notes {
		if note == d.haltReason {
			reported = true
		}
	}
	if !reported {
		t.Fatalf("the tick after the halt did not report its halt: %v", r.Notes)
	}
	if host.holdTries != holds {
		t.Fatal("the tick after the halt reached the host")
	}
	if store.HaltStateAt(d.Store.Path).Present {
		t.Fatal("the tick after the halt published a marker again")
	}
}
