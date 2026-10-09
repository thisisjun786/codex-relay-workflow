package daemon

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1071: the kept-acknowledgement confirmation and the recipient-turn check hand a failure of the corrupting
// class back to the tick, which halts the store on it (I-564) and ends before the later steps write anything.

// With a kept acknowledgement store whose evidence table is damaged, the confirmation's listing meets it.
func TestHaltHostCheck_aCorruptingKeptAckListingHaltsTheTickBeforeTheDelivery(t *testing.T) {
	t.Parallel()
	host := &idleHost{}
	d, s := idleDaemon(t, host)
	testsupport.DamageTable(t, s.DB, s.Path, "ack_evidence")
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting confirmation returned %v instead of halting the store", err)
	}
	if r.Delivered != 0 || r.Deferred != 0 || host.holdTries != 0 {
		t.Fatalf("the tick went on after the halt: delivered %d, deferred %d, holds %d", r.Delivered, r.Deferred, host.holdTries)
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteObservation)
	idleWakeAssertNextTickWritesNothing(t, d, host)
}

// lostTurnHost lists no turn of the recipient and holds no token of the attempt, so the host has lost the delivery,
// and damages the journal when it is asked, so the loss is the write that meets it.
type lostTurnHost struct {
	*idleHost
	damage func()
}

func (h *lostTurnHost) FindDispatchedTurn(context.Context, string, string, float64) (delivery.TurnPresence, error) {
	h.damage()
	return delivery.TurnPresence{Finding: delivery.TurnAbsent, Stop: "listing_end"}, nil
}

func (h *lostTurnHost) FindTokenSince(context.Context, string, string, []string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{Exhausted: true}, nil
}

// A delivered completion the host lost: the check settles the loss, and the settlement's write meets the damage.
func TestHaltHostCheck_aCorruptingTurnCheckHaltsTheTickBeforeTheDelivery(t *testing.T) {
	t.Parallel()
	inner := &idleHost{}
	host := &lostTurnHost{idleHost: inner}
	d, s := idleDaemon(t, inner)
	d.Host = host
	host.damage = func() { testsupport.DamageTable(t, s.DB, s.Path, "journal") }
	exec(t, s, "UPDATE deliveries SET state='dispatched', attempt_count=1, next_eligible_at=NULL, dispatch_turn_id='turn-lost'")
	exec(t, s, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,record,sent_at,observed_at) VALUES('req-904',?,1,'completion_event','settled','dispatched','{\"turnId\":\"turn-lost\"}','2023-11-14T21:00:00+00:00','2023-11-14T21:00:00Z')", idleEvent)
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting turn check returned %v instead of halting the store", err)
	}
	if r.TurnsLost != 0 || inner.holdTries != 0 {
		t.Fatalf("the tick went on after the halt: lost %d, holds %d, notes %v", r.TurnsLost, inner.holdTries, r.Notes)
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteWrite)
	idleWakeAssertNextTickWritesNothing(t, d, inner)
}
