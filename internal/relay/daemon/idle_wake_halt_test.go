package daemon

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-904 (correction, d1): a store failure in the idle pass ends the tick with its error, before the
// delivery pass and the supervisor channel write anything. The baseline has no daemon halt yet (CRW-848
// is not merged here), so the error return is the stop: no statement follows a failed one in the tick.

// idleWakeHaltDamage breaks the tables the idle pass writes and reads, so the pass's own statement
// fails with a store error. keepWake leaves the wake table in a shape its insert refuses, so the
// failure is the wake write while the waiting-head read still answers; without it the wake table is
// gone and the failure is the head read.
func idleWakeHaltDamage(t *testing.T, s *store.Store, keepWake bool) {
	t.Helper()
	exec(t, s, "DROP TABLE delivery_wakes")
	if keepWake {
		exec(t, s, "CREATE TABLE delivery_wakes (event_id TEXT PRIMARY KEY, spent_at TEXT)")
		return
	}
	exec(t, s, "ALTER TABLE deliveries RENAME TO deliveries_intact")
}

// TestIdleWakeHalt_a_failed_wake_write_ends_the_tick_before_delivery: the wake is the idle pass's own
// write, so its failure ends the tick, and the delivery pass that would attempt the waiting head does
// not run.
func TestIdleWakeHalt_a_failed_wake_write_ends_the_tick_before_delivery(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}}
	d, s := idleDaemon(t, host)
	idleWakeHaltDamage(t, s, true)
	if _, err := d.Tick(context.Background()); err == nil {
		t.Fatal("a failed wake write returned no error: the tick went on into the delivery pass")
	}
	if n := busyAnswers(t, s); n != 0 {
		t.Fatalf("the delivery pass ran after the failed wake write (%d busy answers)", n)
	}
}

// TestIdleWakeHalt_a_failed_head_read_ends_the_tick: the waiting heads are read out of the store, so a
// failed read ends the tick, and no subscription is opened on a store that could not be read.
func TestIdleWakeHalt_a_failed_head_read_ends_the_tick(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s := idleDaemon(t, host)
	idleWakeHaltDamage(t, s, false)
	if _, err := d.Tick(context.Background()); err == nil {
		t.Fatal("a failed read of the waiting heads returned no error")
	}
	if len(host.holds) != 0 {
		t.Fatalf("a subscription was opened on a store that could not be read: %v", host.holds)
	}
}

// TestIdleWakeHalt_no_failure_leaves_the_tick_alone: a healthy store answers the tick without error.
func TestIdleWakeHalt_no_failure_leaves_the_tick_alone(t *testing.T) {
	t.Parallel()
	host := &idleHost{reports: []delivery.IdleReport{{ThreadID: idleThread, Status: "idle"}}}
	d, _ := idleDaemon(t, host)
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatalf("a healthy tick returned %v", err)
	}
}
