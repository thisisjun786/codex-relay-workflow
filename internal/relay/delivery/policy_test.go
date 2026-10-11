package delivery

import (
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// holdsLive makes task the owner of a project, the live parent binding that makes the relationships it
// is the parent of relationships of class orchestrator.
func (f *fixture) holdsLive(task, project string) {
	f.t.Helper()
	_, err := execSQL(f.ctx, f.store, "INSERT INTO scope_bindings (binding_id, role, scope_kind, scope_key, task_id, host_id, cwd, cxc_session, status, revision, supersedes, superseded_by, handover_note, created_at, updated_at) VALUES (?,?,?,?,?,?,NULL,NULL,?,1,NULL,NULL,NULL,?,?)",
		"binding-"+project, "parent", "project", project, task, host, "active", f.clock.ISO(), f.clock.ISO())
	mustDo(f.t, err)
}

// spendMessage leaves transports transport attempts of one supervisor message of rid to recipient in
// now's hour window, each of which may have gone.
func (f *fixture) spendMessage(rid, recipient, message string, transports int, now float64) {
	f.t.Helper()
	stamp := store.SendStamp(math.Floor(now/3600) * 3600)
	_, err := execSQL(f.ctx, f.store, "INSERT INTO supervisor_messages (message_id, obligation_id, obligation_kind, relationship_id, purpose, kind, sender_task_id, recipient_task_id, subject, packet, state, staged_at, updated_at) VALUES (?,?,'report',?,'p','k','sender',?,'s','{}','sent',?,?)", message, message, rid, recipient, stamp, stamp)
	mustDo(f.t, err)
	for i := 1; i <= transports; i++ {
		_, err = execSQL(f.ctx, f.store, "INSERT INTO supervisor_attempts (request_id, message_id, attempt_no, message, state, send_attempted, retry_safe, record, sent_at, transport_started_at, observed_at) VALUES (?,?,?,'m','dispatched','yes',0,'{}',?,?,?)", fmt.Sprintf("%s-%d", message, i), message, i, stamp, stamp, stamp)
		mustDo(f.t, err)
	}
}

// pacingOf is the reason, sends and cap of the budget that holds the fixture's relationship, or "" when
// nothing does.
func (f *fixture) pacingOf() (reason string, sends, capacity int64) {
	f.t.Helper()
	p, err := f.delivery.pacing(f.ctx, f.rid, parent, f.clock.Now())
	mustDo(f.t, err)
	if p == nil {
		return "", 0, 0
	}
	sends, _ = p.Get("sends").(int64)
	capacity, _ = p.Get("cap").(int64)
	return pyjson.Text(p.Get("reason")), sends, capacity
}

func (f *fixture) sendsOf(rid, recipient string) int64 {
	f.t.Helper()
	n, err := f.store.RelationshipSends(f.ctx, rid, recipient, math.Floor(f.clock.Now()/3600)*3600)
	mustDo(f.t, err)
	return n
}

// spentOf is the hour count the due-delivery read joins (store.RelationshipSpentSQL).
func (f *fixture) spentOf(rid, recipient string) int64 {
	f.t.Helper()
	window := math.Floor(f.clock.Now()/3600) * 3600
	row := f.one("SELECT COALESCE((SELECT spent FROM "+store.RelationshipSpentSQL+" WHERE relationship_id = ? AND recipient_task_id = ?), 0) AS c",
		append(store.RelationshipSendsArgs(window), rid, recipient)...)
	return row.I("c")
}

// The hour budget counts the distinct events a relationship has sent to a recipient, not the attempts
// made: one event tried a hundred times is one send, and the cap is reached by its cap-th distinct event.
func TestPacingCountsDistinctEvents(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	now := f.clock.Now()
	capacity := int(f.delivery.Policy.MaxSendsPerRelationshipPerHour)

	f.spendAttempts(f.rid, parent, "seed-loop", 5*capacity, now)
	if got, spent := f.sendsOf(f.rid, parent), f.spentOf(f.rid, parent); got != 1 || spent != 1 {
		t.Fatalf("%d attempts of one event count %d (due read %d), want 1", 5*capacity, got, spent)
	}
	if reason, _, _ := f.pacingOf(); reason != "" {
		t.Fatalf("one event tried %d times holds the relationship with %q", 5*capacity, reason)
	}
	f.spendMessage(f.rid, parent, "m-loop", 4, now)
	if got, spent := f.sendsOf(f.rid, parent), f.spentOf(f.rid, parent); got != 2 || spent != 2 {
		t.Fatalf("one supervisor message sent four times counts %d (due read %d), want 1 more", got, spent)
	}

	// capacity-1 distinct events in all, two attempts each for the new ones: still under the cap.
	for i := 2; i < capacity-1; i++ {
		f.spendAttempts(f.rid, parent, fmt.Sprintf("seed-%d", i), 2, now)
	}
	if reason, _, _ := f.pacingOf(); reason != "" {
		t.Fatalf("%d distinct events hold the relationship with %q", capacity-1, reason)
	}
	if !slices.Contains(f.eligible(), event) {
		t.Fatal("a relationship under its cap of distinct events has no due delivery")
	}
	f.spendAttempts(f.rid, parent, "seed-last", 1, now)
	reason, sends, got := f.pacingOf()
	if reason != HourlyCap || sends != int64(capacity) || got != int64(capacity) {
		t.Fatalf("%d distinct events: reason %q sends %d cap %d, want %s at %d", capacity, reason, sends, got, HourlyCap, capacity)
	}
	if spent := f.spentOf(f.rid, parent); spent != int64(capacity) {
		t.Fatalf("the due read counts %d, want %d", spent, capacity)
	}
	if slices.Contains(f.eligible(), event) {
		t.Fatal("a relationship that has sent its cap of distinct events still has a due delivery")
	}
}

// The attempts of one event stay bounded per event: a delivery whose pre-send attempts reach MaxAttempts
// is held at attempt_cap and is not due again, although it spent none of the relationship's hour (the
// hour counts distinct events, so it is not what stops a repeated event).
func TestPerEventAttemptCapKept(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	f.delivery.Policy.MaxAttempts = 3
	event := f.queuedEvent(regOpts{})
	for i := 0; i < 3; i++ {
		f.host.script = []string{"read_fail"}
		f.clock.Advance(100000)
		f.mustAttempt(event, at(f.clock.Now()))
	}
	if got := f.row(event).S("hold_reason"); got != AttemptCap {
		t.Fatalf("hold %q after %d failed attempts, want %s", got, 3, AttemptCap)
	}
	if n := f.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", event); n != 3 {
		t.Fatalf("%d attempts were made, want the cap 3", n)
	}
	f.clock.Advance(100000)
	if slices.Contains(f.eligible(), event) {
		t.Fatal("an event held at its attempt cap is due again")
	}
	if reason, sends, _ := f.pacingOf(); reason != "" || sends != 0 {
		t.Fatalf("pre-send failures of one event spent the hour: %q %d", reason, sends)
	}
}

// A relationship of class orchestrator has its own hour cap: its parent holds the live parent binding of
// a project. The other relationships keep theirs, and a parent that no longer holds the binding falls back
// to the ordinary class.
func TestOrchestratorClassBudget(t *testing.T) {
	t.Parallel()
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	now := f.clock.Now()
	ordinary, orchestrator := int(f.delivery.Policy.MaxSendsPerRelationshipPerHour), int(f.delivery.Policy.MaxSendsPerOrchestratorRelationshipPerHour)
	if orchestrator <= ordinary {
		t.Fatalf("the orchestrator class has cap %d, not above the ordinary %d", orchestrator, ordinary)
	}

	f.spendHour(f.rid, parent, ordinary, now)
	if reason, _, capacity := f.pacingOf(); reason != HourlyCap || capacity != int64(ordinary) || slices.Contains(f.eligible(), event) {
		t.Fatalf("ordinary class: %q cap %d, due %v; want %s at %d and nothing due", reason, capacity, f.eligible(), HourlyCap, ordinary)
	}

	f.holdsLive(parent, "PROJECT-1")
	if reason, _, _ := f.pacingOf(); reason != "" {
		t.Fatalf("orchestrator class after %d sends is held by %q, want room under %d", ordinary, reason, orchestrator)
	}
	if !slices.Contains(f.eligible(), event) {
		t.Fatal("an orchestrator's relationship at the ordinary cap has no due delivery")
	}
	for i := ordinary; i < orchestrator; i++ {
		f.spendAttempts(f.rid, parent, fmt.Sprintf("seed-orchestrator-%d", i), 1, now)
	}
	if reason, sends, capacity := f.pacingOf(); reason != HourlyCap || sends != int64(orchestrator) || capacity != int64(orchestrator) || slices.Contains(f.eligible(), event) {
		t.Fatalf("orchestrator cap: %q sends %d cap %d due %v, want %s at %d and nothing due", reason, sends, capacity, f.eligible(), HourlyCap, orchestrator)
	}

	// The parent gives the project up: the relationship is ordinary again, and its sends are over that cap.
	_, err := execSQL(f.ctx, f.store, "UPDATE scope_bindings SET status = 'released' WHERE task_id = ?", parent)
	mustDo(t, err)
	if reason, _, capacity := f.pacingOf(); reason != HourlyCap || capacity != int64(ordinary) {
		t.Fatalf("after the binding ended: %q cap %d, want %s at %d", reason, capacity, HourlyCap, ordinary)
	}
}
