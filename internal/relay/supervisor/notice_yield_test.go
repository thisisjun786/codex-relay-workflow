package supervisor

import (
	"errors"
	"fmt"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-905 (section 82 decision 2, docs/relay/invariants.md I-216): the notice channel yields the
// recipient's line to a delivery that waits out a busy backoff. A notice to a recipient whose line
// is held is refused not_claimable - the refusal name that already exists - and it becomes
// claimable when that head's backoff ends or the head is delivered. A supervisor message keeps the
// oldest-of-the-claimable rule and is untouched.
//
// The head is a real deliveries row: the one the delivery path's busyHeadSQL names, so these tests
// ask the shared predicate rather than a second reading of it.

// noticeYieldWorld is the notice fixture with the delivery line the yield is about.
type noticeYieldWorld struct {
	*noticeWorld
	now float64
}

func newNoticeYieldWorld(t *testing.T) *noticeYieldWorld {
	t.Helper()
	w := newNoticeWorld(t)
	w.c.Settings = &delivery.TaskSettings{}
	w.channel.Host = &sendHost{status: "idle"}
	return &noticeYieldWorld{noticeWorld: w, now: nsNow}
}

// line inserts the delivery that holds recipient's line until due: the row busyHeadSQL names, a
// delivery waiting out a busy backoff on an active relationship and a final event.
func (w *noticeYieldWorld) line(t *testing.T, recipient string, due float64) {
	t.Helper()
	w.exec(t, "INSERT INTO deliveries (event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,next_eligible_at,created_at,updated_at) VALUES ('event-1','rel-1','receipt',?,?, 'deferred_busy',1,?,'t','t')", recipient, recipient, due)
}

// stagedNotice stages the fixture's fault notice and returns its message id.
func (w *noticeYieldWorld) stagedNotice(t *testing.T) string {
	t.Helper()
	w.seed(t, nsSeed{signature: nsProjectSig, scope: nsProjectScope, state: "reserved", lease: nsNow + 300})
	answer, err := w.channel.StageNotice(w.ctx, w.facts(t, nsID))
	if err != nil {
		t.Fatalf("stage notice: %v", err)
	}
	return answer["messageId"].(string)
}

func (w *noticeYieldWorld) message(t *testing.T, id string) store.SupervisorMessagesRow {
	t.Helper()
	row, err := w.c.Get(w.ctx, id)
	if err != nil {
		t.Fatalf("read message %s: %v", id, err)
	}
	return row
}

func (w *noticeYieldWorld) attempts(t *testing.T, id string) int {
	t.Helper()
	attempts, err := w.s.SupervisorAttempts(w.ctx, id)
	if err != nil {
		t.Fatalf("attempts of %s: %v", id, err)
	}
	return len(attempts)
}

func noticeYieldRefusal(t *testing.T, err error) Refusal {
	t.Helper()
	var refusal Refusal
	if !errors.As(err, &refusal) || refusal.Reason != "not_claimable" {
		t.Fatalf("want a not_claimable refusal, got %v", err)
	}
	return refusal
}

// noticeYieldDeliverySQL seeds a delivery that holds recipient's line until due.
func noticeYieldDeliverySQL(recipient string, due float64) string {
	return fmt.Sprintf("INSERT INTO deliveries (event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,next_eligible_at,created_at,updated_at) VALUES ('event-1','rel-1','receipt','%s','%s','deferred_busy',1,%v,'t','t')", recipient, recipient, due)
}

func TestNoticeYield_a_notice_waits_while_a_delivery_holds_the_line_under_a_busy_backoff(t *testing.T) {
	t.Parallel()
	// Given: a staged notice to the supervisor, and a delivery to that supervisor waiting out a
	// busy backoff.
	w := newNoticeYieldWorld(t)
	id := w.stagedNotice(t)
	w.line(t, "supervisor", w.now+60)
	before := w.message(t, id)
	// When: the notice is attempted.
	err := w.channel.Attempt(w.ctx, id, w.now, "relay")
	// Then: it is refused not_claimable, nothing is sent, no attempt is recorded and the message is
	// left exactly where it was.
	refusal := noticeYieldRefusal(t, err)
	after := w.message(t, id)
	if after != before || w.attempts(t, id) != 0 || after.State != "queued" {
		t.Fatalf("refusal %+v, row %+v -> %+v, attempts %d", refusal, before, after, w.attempts(t, id))
	}
}

func TestNoticeYield_a_notice_is_claimable_once_the_heads_backoff_ends(t *testing.T) {
	t.Parallel()
	// Given: the same notice, with the holding delivery's backoff already past.
	w := newNoticeYieldWorld(t)
	id := w.stagedNotice(t)
	w.line(t, "supervisor", w.now-1)
	// When: the notice is attempted.
	if err := w.channel.Attempt(w.ctx, id, w.now, "relay"); err != nil {
		t.Fatalf("notice held after the head's backoff ended: %v", err)
	}
	// Then: it goes out.
	if after := w.message(t, id); after.State != "dispatched" || w.attempts(t, id) != 1 {
		t.Fatalf("row %+v, attempts %d", after, w.attempts(t, id))
	}
}

func TestNoticeYield_a_notice_is_claimable_once_the_head_is_delivered(t *testing.T) {
	t.Parallel()
	// Given: the same notice, with the holding delivery already sent.
	w := newNoticeYieldWorld(t)
	id := w.stagedNotice(t)
	w.line(t, "supervisor", w.now+60)
	w.exec(t, "UPDATE deliveries SET state='dispatched'")
	// When: the notice is attempted.
	if err := w.channel.Attempt(w.ctx, id, w.now, "relay"); err != nil {
		t.Fatalf("notice held after the head was delivered: %v", err)
	}
	// Then: it goes out.
	if after := w.message(t, id); after.State != "dispatched" || w.attempts(t, id) != 1 {
		t.Fatalf("row %+v, attempts %d", after, w.attempts(t, id))
	}
}

func TestNoticeYield_a_supervisor_message_is_unchanged_by_a_busy_backoff_head(t *testing.T) {
	t.Parallel()
	// Given: an ordinary supervisor message to the same recipient, and the same holding delivery.
	f := fixture24(t)
	f.c.Settings = &delivery.TaskSettings{}
	id := stagedID(t, f)
	sweepExec(t, f, noticeYieldDeliverySQL("supervisor", guardNow+60))
	// When: the message is attempted.
	record, err := f.c.Attempt(f.ctx, id, newGuardHost(), guardNow)
	// Then: it is claimed and sent as today: only the notice channel yields.
	if err != nil || record == nil || record["deliveryState"] != "dispatched" {
		t.Fatalf("record %v err %v", record, err)
	}
}

func TestNoticeYield_a_busy_backoff_head_for_another_recipient_does_not_hold_a_notice(t *testing.T) {
	t.Parallel()
	// Given: a staged notice to the supervisor, and a delivery holding a DIFFERENT recipient's line.
	w := newNoticeYieldWorld(t)
	id := w.stagedNotice(t)
	w.line(t, "some-other-task", w.now+60)
	// When: the notice is attempted.
	if err := w.channel.Attempt(w.ctx, id, w.now, "relay"); err != nil {
		t.Fatalf("a head for another recipient held this notice: %v", err)
	}
	// Then: it goes out: the yield is per recipient.
	if after := w.message(t, id); after.State != "dispatched" || w.attempts(t, id) != 1 {
		t.Fatalf("row %+v, attempts %d", after, w.attempts(t, id))
	}
}
