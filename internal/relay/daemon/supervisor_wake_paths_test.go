package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1334 (evaluation bbcacfcf d1): the branches of the daemon's idle pass the supervisor channel
// added - one recipient waiting in both channels, the second hold pass after the supervisor pass, and
// a store failure of the supervisor wake - each pinned by a test of its own.

// seedSharedHead adds a delivery head waiting out a busy backoff until deadline whose recipient is the
// supervisor the fixture's staged message is addressed to, so that recipient waits in both channels.
func seedSharedHead(t *testing.T, s *store.Store, deadline float64) {
	t.Helper()
	seedHeadFor(t, s, "shared", supWakeRecipient, deadline)
}

// seedHeadFor adds relationship rel-<name> whose parent is recipient, and its completion delivery
// ev-<name> waiting out a busy backoff until deadline.
func seedHeadFor(t *testing.T, s *store.Store, name, recipient string, deadline float64) {
	t.Helper()
	const stamp = "2023-11-14T22:13:20Z"
	rel, event := "rel-"+name, "ev-"+name
	exec(t, s, "INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,parent_cwd,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES(?,?,'active',?,'host','/parent',?,'host','/child',1,'[]',?,?,?)", rel, "REL-"+name, recipient, "child-"+name, `["`+recipient+`"]`, stamp, stamp)
	exec(t, s, "INSERT INTO generations(relationship_id,execution_generation,dispatch_request_id,anchor_state,dispatch_turn_id,reason,opened_at,bound_at) VALUES(?,1,?,'bound',?,'initial_assignment',?,?)", rel, "dispatch-"+rel, "anchor-"+name, stamp, stamp)
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES(?,?,1,?,'ready_for_review','child',?,?,'completed','{}','final',?,?)", event, rel, "rev-"+name, "child-"+name, "turn-"+name, stamp, stamp)
	exec(t, s, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,next_eligible_at,created_at,updated_at) VALUES(?,?,'completion_event',?,?,'deferred_busy',0,?,?,?)", event, rel, recipient, recipient, deadline, stamp, stamp)
}

// tickAt runs one tick at the instant at and fails the test on a tick error.
func tickAt(t *testing.T, d *Daemon, at float64) Report {
	t.Helper()
	d.Clock.(*delivery.FakeClock).T = at
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("tick at %.0f: %v", at, err)
	}
	return r
}

func TestIdleWake_a_recipient_waiting_in_both_channels_has_one_subscription(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s, id := supervisorWakeDaemon(t, host)
	seedSharedHead(t, s, idleNow+300)
	exec(t, s, "UPDATE supervisor_messages SET state = 'deferred_busy', next_eligible_at = ? WHERE message_id = ?", idleNow+40, id)
	tickAt(t, d, idleNow)
	tickAt(t, d, idleNow+1)
	if host.holdTries != 1 || !slices.Equal(host.holds, []string{supWakeRecipient}) {
		t.Fatalf("holds %v after %d tries, want the one recipient held once for both channels", host.holds, host.holdTries)
	}
	// The supervisor message no longer waits, the delivery still does: the subscription stays.
	exec(t, s, "UPDATE supervisor_messages SET state = 'dispatched', next_eligible_at = NULL WHERE message_id = ?", id)
	tickAt(t, d, idleNow+2)
	if len(host.releases) != 0 {
		t.Fatalf("releases %v while the delivery still waits on the recipient", host.releases)
	}
	// Neither channel waits: the one subscription is released once.
	exec(t, s, "UPDATE deliveries SET state = 'dispatched', next_eligible_at = NULL WHERE event_id = 'ev-shared'")
	tickAt(t, d, idleNow+3)
	if !slices.Equal(host.releases, []string{supWakeRecipient}) {
		t.Fatalf("releases %v, want the recipient's one subscription released once", host.releases)
	}
}

func TestIdleWake_a_refused_shared_hold_waits_for_the_earliest_deadline_of_either_channel(t *testing.T) {
	t.Parallel()
	host := &idleHost{holdErr: errors.New("the host refused the resume")}
	d, s, id := supervisorWakeDaemon(t, host)
	// The delivery head (read first) is due at +300, the supervisor message at +40: the refused hold is
	// tried again at the earlier of the two, the supervisor's.
	seedSharedHead(t, s, idleNow+300)
	exec(t, s, "UPDATE supervisor_messages SET state = 'deferred_busy', next_eligible_at = ? WHERE message_id = ?", idleNow+40, id)
	tickAt(t, d, idleNow)
	if host.holdTries != 1 {
		t.Fatalf("%d hold tries in the first tick, want one", host.holdTries)
	}
	tickAt(t, d, idleNow+39)
	if host.holdTries != 1 {
		t.Fatalf("the refused hold was retried before the supervisor's deadline: %d tries", host.holdTries)
	}
	tickAt(t, d, idleNow+41)
	if host.holdTries != 2 {
		t.Fatalf("the refused hold was not retried after the supervisor's deadline (+40), only the delivery's (+300) was waited for: %d tries", host.holdTries)
	}
}

func TestIdleWake_a_capped_supervisor_message_gives_a_shared_refused_hold_no_deadline(t *testing.T) {
	t.Parallel()
	host := &idleHost{holdErr: errors.New("the host refused the resume")}
	d, s, id := supervisorWakeDaemon(t, host)
	// The supervisor message is held at busy_cap (no deadline: only an edge releases it), the delivery
	// head is due at +300. The refused hold waits for the delivery's deadline, not for none at all.
	seedSharedHead(t, s, idleNow+300)
	exec(t, s, "UPDATE supervisor_messages SET state = 'deferred_busy', hold_reason = ?, next_eligible_at = ? WHERE message_id = ?", delivery.BusyCap, idleNow+40, id)
	tickAt(t, d, idleNow)
	for _, at := range []float64{idleNow + 16, idleNow + 60, idleNow + 299} {
		tickAt(t, d, at)
		if host.holdTries != 1 {
			t.Fatalf("at %.0f the refused hold was retried %d times: the capped message's zero deadline was taken as the earliest", at, host.holdTries)
		}
	}
	tickAt(t, d, idleNow+301)
	if host.holdTries != 2 {
		t.Fatalf("the refused hold was not retried at the delivery's deadline: %d tries", host.holdTries)
	}
}

// sendingIdleHost is idleHost whose recipient is idle and takes a supervisor message: the send the
// second hold pass follows.
type sendingIdleHost struct {
	*idleHost
	sends []string
}

func (h *sendingIdleHost) SendMessage(_ context.Context, id, _, message string, _ *delivery.TaskSettings) (delivery.Obj, error) {
	h.sends = append(h.sends, message)
	return delivery.Obj{{Key: "status", Value: "accepted"}, {Key: "requestId", Value: id}, {Key: "turnId", Value: "turn-supervisor-1"}}, nil
}

func TestIdleWake_the_tick_that_sends_the_last_waiting_supervisor_message_releases_its_subscription(t *testing.T) {
	t.Parallel()
	inner := &idleHost{}
	host := &sendingIdleHost{idleHost: inner}
	d, s, id := supervisorWakeDaemon(t, inner)
	d.Host = host
	tickAt(t, d, idleNow)
	if supervisorDeferrals(t, s, id) != 1 || !slices.Equal(inner.holds, []string{supWakeRecipient}) {
		t.Fatalf("the first tick: %d busy answers, holds %v, want one deferral and the recipient held", supervisorDeferrals(t, s, id), inner.holds)
	}
	// The recipient goes idle and says so: the same tick wakes the message, sends it, and - in the hold
	// pass after the supervisor pass - releases the subscription nothing waits on any more.
	inner.status = "idle"
	inner.reports = []delivery.IdleReport{{ThreadID: supWakeRecipient, Status: "idle"}}
	tickAt(t, d, idleNow+1)
	if len(host.sends) != 1 {
		t.Fatalf("%d sends in the woken tick, want the message sent", len(host.sends))
	}
	if n := count(t, s, "SELECT COUNT(*) FROM supervisor_messages WHERE message_id = ? AND state = 'deferred_busy'", id); n != 0 {
		t.Fatal("the sent message still waits out its backoff")
	}
	if !slices.Equal(inner.releases, []string{supWakeRecipient}) {
		t.Fatalf("releases %v at the end of the sending tick, want the recipient's subscription released in that tick", inner.releases)
	}
}

// The supervisor wake is the idle pass's own store work: a corrupting failure of it ends the tick in the
// idle pass, as the delivery wake's does - no later report of the same batch is applied, and the delivery
// pass and the hold pass do not run. The failing statement is the wake's read of the waiting messages,
// so the marker names the observation site (store.SiteOf: the step that met the failure decides read or
// write, CRW-945).
func TestIdleWakeHalt_a_failed_supervisor_wake_ends_the_tick_in_the_idle_pass(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s, _ := supervisorWakeDaemon(t, host)
	tickAt(t, d, idleNow)
	tries := host.holdTries
	// Another recipient's delivery head waits out its backoff; its edge is reported after the
	// supervisor's in the same batch, so it is applied only if the pass goes on past the failure.
	seedHeadFor(t, s, "later", "parent-later", idleNow+300)
	host.reports = []delivery.IdleReport{{ThreadID: supWakeRecipient, Status: "idle"}, {ThreadID: "parent-later", Status: "idle"}}
	d.idle.beforeIdle = func() { testsupport.DamageTable(t, s.DB, s.Path, "supervisor_messages") }
	d.Clock.(*delivery.FakeClock).T = idleNow + 1
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting supervisor wake returned %v instead of halting the store", err)
	}
	if r.Delivered != 0 || r.Deferred != 0 {
		t.Fatalf("the delivery pass ran after the failed supervisor wake: delivered %d, deferred %d", r.Delivered, r.Deferred)
	}
	if host.holdTries != tries {
		t.Fatalf("the hold pass ran after the failed supervisor wake: %d tries, want %d", host.holdTries, tries)
	}
	// The deliveries table is not damaged: the later report's head was not woken.
	if n := count(t, s, "SELECT COUNT(*) FROM deliveries WHERE event_id = 'ev-later' AND next_eligible_at = ?", idleNow+300); n != 1 {
		t.Fatal("the report after the failed supervisor wake was still applied")
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteObservation)
	d.idle.beforeIdle = nil
	idleWakeAssertNextTickWritesNothing(t, d, host)
}
