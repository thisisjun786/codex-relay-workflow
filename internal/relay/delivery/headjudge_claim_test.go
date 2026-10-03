package delivery

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

// CRW-416: a claim judges its event once, in the transaction that claims it, and a superseded
// event is recorded and refused from that same transaction. These tests hold the outcomes of
// that path (they pass on the code before the change as they do after it): what is stored for a
// superseded revision, what is stored when the delivery is no longer queued, what the caller
// reads when the commit fails, and that a current event and an event with no head are claimed.

// addDelivery gives revision i of the world a completion delivery in the given state.
func (w *headWorld) addDelivery(i int, state string) string {
	w.tb.Helper()
	event := headEvent(i)
	stamp := hotStamp(fmt.Sprint(headEpoch))
	if _, err := execSQL(w.ctx, w.store, "INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id, recipient_thread_id, state, attempt_count, next_eligible_at, created_at, updated_at) VALUES (?,?,?,?,?,?,0,NULL,"+stamp+","+stamp+")", event, headRelationship, Completion, headRecipient, headRecipient, state); err != nil {
		w.tb.Fatal(err)
	}
	return event
}

func (w *headWorld) row(query string, args ...any) Row {
	w.tb.Helper()
	r, err := one(w.ctx, w.store, query, args...)
	if err != nil {
		w.tb.Fatal(err)
	}
	return r
}

func (w *headWorld) count(query string, args ...any) int64 {
	w.tb.Helper()
	return w.row(query, args...).I("c")
}

// The first three revisions of a chain are each replaced by a final successor.
func TestClaimOfASupersededRevision(t *testing.T) {
	t.Parallel()
	w := newHeadWorld(t, shapeChain, 4)

	t.Run("is recorded and refused", func(t *testing.T) {
		old := w.addDelivery(0, Queued)
		_, err := w.d.claim(w.ctx, old, w.now, "test", headRecipient)
		var gone *superseded
		if !errors.As(err, &gone) || gone.reason != SupersededRevision {
			t.Fatalf("claim of a revision a final successor replaces: %v", err)
		}
		row := w.row("SELECT state, hold_reason, attempt_count FROM deliveries WHERE event_id = ?", old)
		if row.S("state") != Superseded || row.S("hold_reason") != SupersededRevision || row.I("attempt_count") != 0 {
			t.Fatalf("the delivery after the refusal: %v", row)
		}
		if n := w.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'delivery_superseded' AND subject = ?", old); n != 1 {
			t.Fatalf("delivery_superseded journal rows: %d", n)
		}
		if n := w.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", old); n != 0 {
			t.Fatalf("a refused claim wrote %d attempts", n)
		}
	})

	// Attempt checks the state before it observes the host, and another claim can move the
	// delivery to sending in between: the supersession is then only noted, the state is kept.
	t.Run("already sending is only noted", func(t *testing.T) {
		old := w.addDelivery(1, Sending)
		_, err := w.d.claim(w.ctx, old, w.now, "test", headRecipient)
		var gone *superseded
		if !errors.As(err, &gone) || gone.reason != SupersededRevision {
			t.Fatalf("claim of a sending revision a final successor replaces: %v", err)
		}
		row := w.row("SELECT state, hold_reason, attempt_count FROM deliveries WHERE event_id = ?", old)
		if row.S("state") != Sending || !row.N("hold_reason") || row.I("attempt_count") != 0 {
			t.Fatalf("the delivery after the refusal: %v", row)
		}
		note := w.row("SELECT reason, applied FROM delivery_supersession WHERE event_id = ?", old)
		if note == nil || note.S("reason") != SupersededRevision || note.I("applied") != 0 {
			t.Fatalf("the note: %v", note)
		}
		if n := w.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'delivery_superseded' AND subject = ?", old); n != 0 {
			t.Fatalf("delivery_superseded journal rows: %d", n)
		}
	})

	// A claim whose commit fails reports the failure and records nothing; it is not a supersession.
	t.Run("a failed commit is reported as the failure", func(t *testing.T) {
		old := w.addDelivery(2, Queued)
		ctx, cancel := context.WithCancel(w.ctx)
		defer cancel()
		w.store.SetFaultHook(cancel)
		defer w.store.SetFaultHook(nil)
		_, err := w.d.claim(ctx, old, w.now, "test", headRecipient)
		var gone *superseded
		if err == nil || errors.As(err, &gone) || !errors.Is(err, context.Canceled) {
			t.Fatalf("claim with a commit that fails: %v", err)
		}
		w.store.SetFaultHook(nil)
		row := w.row("SELECT state, hold_reason FROM deliveries WHERE event_id = ?", old)
		if row.S("state") != Queued || !row.N("hold_reason") {
			t.Fatalf("the delivery after the failed commit: %v", row)
		}
		if n := w.count("SELECT COUNT(*) AS c FROM journal WHERE kind = 'delivery_superseded' AND subject = ?", old); n != 0 {
			t.Fatalf("delivery_superseded journal rows: %d", n)
		}
	})
}

func TestClaimOfTheHeadOrOfAnEventWithNoHeadClaims(t *testing.T) {
	t.Parallel()
	for _, shape := range []headShape{shapeChain, shapeLoose} {
		t.Run(string(shape), func(t *testing.T) {
			w := newHeadWorld(t, shape, 5)
			got, err := w.claim()
			if err != nil || got.attemptNo != 1 {
				t.Fatalf("claim: %+v, %v", got, err)
			}
			if state := w.row("SELECT state FROM deliveries WHERE event_id = ?", w.tip).S("state"); state != Sending {
				t.Fatalf("state %q", state)
			}
			if n := w.count("SELECT COUNT(*) AS c FROM attempts WHERE event_id = ?", w.tip); n != 1 {
				t.Fatalf("attempts %d", n)
			}
		})
	}
}
