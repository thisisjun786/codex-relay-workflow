package daemon

import (
	"context"
	"runtime/debug"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-1071: the kept-acknowledgement confirmation and the recipient-turn check hand a failure of the corrupting
// class back to the tick, which halts the store on it (I-564) and ends before the later steps write anything.

// seedEvent adds a completion event of rel-904 with its delivery, in the state given, and the attempt that made it:
// request req-<event>, attempt 1, settled as the state says.
func seedEvent(t *testing.T, s *store.Store, event, state string) {
	t.Helper()
	const stamp = "2023-11-14T22:13:20Z"
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES(?,'rel-904',1,?,'ready_for_review','child','child-904',?,'completed','{}','final',?,?)", event, "rev-"+event, "turn-"+event, stamp, stamp)
	exec(t, s, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,dispatch_turn_id,created_at,updated_at) VALUES(?,'rel-904','completion_event',?,?,?,1,?,?,?)", event, idleParent, idleThread, state, "dturn-"+event, stamp, stamp)
	exec(t, s, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,record,sent_at,observed_at) VALUES(?,?,1,'completion_event','settled',?,?,'2023-11-14T21:00:00+00:00',?)", "req-"+event, event, state, `{"turnId":"dturn-`+event+`"}`, stamp)
}

// seedKeptAck makes the acknowledgement of event one the relay kept because it could not confirm the send for the
// parent's turn, with its confirmation due at check.
func seedKeptAck(t *testing.T, s *store.Store, event string, check float64) {
	t.Helper()
	seedEvent(t, s, event, delivery.HeldUncertain)
	exec(t, s, "INSERT INTO acks(event_id,record,ack_turn_id,accepted,verified,ack_at) VALUES(?,'{}',?,1,'unverified_turn','2023-11-14T22:13:20Z')", event, "ack-"+event)
	exec(t, s, "INSERT INTO ack_evidence(event_id,tier,attempts,last_reason,next_check_at,observed_at) VALUES(?,'turn',1,?,?,'2023-11-14T22:13:20Z')", event, delivery.DeliveryUnconfirmed, check)
}

// hostCheckHost is a host that has found nothing of the sends and lost every turn it is asked about. Its hooks run when
// a request is read (a confirmation) or a turn is looked up (a recipient-turn check), so a test can damage the store
// at a chosen item, and the lists record which items were reached.
type hostCheckHost struct {
	*idleHost
	onRequest func(id string)
	onTurn    func(turn string)
	// confirmed are the requests the kept-acknowledgement confirmation read, in order.
	confirmed []string
	turns     []string
}

func (h *hostCheckHost) GetOperation(_ context.Context, id string) (delivery.Obj, error) {
	if inConfirmation() {
		h.confirmed = append(h.confirmed, id)
	}
	if h.onRequest != nil {
		h.onRequest(id)
	}
	return nil, nil
}

func (h *hostCheckHost) FindToken(context.Context, string, string, int, bool) (delivery.TokenScan, error) {
	return delivery.TokenScan{Exhausted: true}, nil
}

func (h *hostCheckHost) FindTokenInTurn(context.Context, string, string, string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{Exhausted: true}, nil
}

func (h *hostCheckHost) FindDispatchedTurn(_ context.Context, _ string, turn string, _ float64) (delivery.TurnPresence, error) {
	h.turns = append(h.turns, turn)
	if h.onTurn != nil {
		h.onTurn(turn)
	}
	return delivery.TurnPresence{Finding: delivery.TurnAbsent, Stop: "listing_end"}, nil
}

func (h *hostCheckHost) RecipientFingerprint(context.Context, string) (string, error) {
	return "fingerprint", nil
}

func (h *hostCheckHost) FindTokenSince(context.Context, string, string, []string, int) (delivery.TokenScan, error) {
	return delivery.TokenScan{Exhausted: true}, nil
}

// inConfirmation is whether the host call being answered was made by the kept-acknowledgement confirmation.
func inConfirmation() bool {
	return strings.Contains(string(debug.Stack()), "delivery.ConfirmKeptAcks")
}

// keptAckDaemon is the idle daemon with its one waiting head and kept acknowledgements due for the events given, each
// check one second after the one before.
func keptAckDaemon(t *testing.T, host *hostCheckHost, events ...string) (*Daemon, *store.Store) {
	t.Helper()
	d, s := idleDaemon(t, host.idleHost)
	d.Host = host
	for i, event := range events {
		seedKeptAck(t, s, event, idleNow-100+float64(i))
	}
	return d, s
}

// A kept acknowledgement is due and the evidence table its listing reads is damaged. The tick halts at the observation
// site of that read and goes no further. (The acknowledgement verification reads the same table, so this test alone does
// not tell the two passes apart; the test below does.)
func TestHaltHostCheck_aCorruptingKeptAckListingHaltsTheTickBeforeTheDelivery(t *testing.T) {
	t.Parallel()
	host := &hostCheckHost{idleHost: &idleHost{}}
	d, s := keptAckDaemon(t, host, "ev-kept-1")
	// A delivery the host lost, behind the confirmation: its turn check would count a loss if the tick went on.
	seedEvent(t, s, "ev-905", delivery.Dispatched)
	testsupport.DamageTable(t, s.DB, s.Path, "ack_evidence")
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting confirmation returned %v instead of halting the store", err)
	}
	if r.Delivered != 0 || r.Deferred != 0 || r.AcksVerified != 0 || r.TurnsLost != 0 || host.idleHost.holdTries != 0 || slices.Contains(host.turns, "dturn-ev-905") {
		t.Fatalf("the tick went on after the halt: %+v, turn lookups %q, holds %d", r, host.turns, host.idleHost.holdTries)
	}
	for _, note := range r.Notes {
		if strings.Contains(note, "pending acknowledgement pass failed") {
			t.Fatalf("the acknowledgement verification was entered after the confirmation met the damage: %v", r.Notes)
		}
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteObservation)
	idleWakeAssertNextTickWritesNothing(t, d, host.idleHost)
}

// Two confirmations are made before the damage: the first is an ordinary failure and stays its note, the second meets
// the damage. The tick keeps the note it made, halts there, and goes no further: it never reads the confirmation behind
// the damaged one, never verifies the kept acknowledgements (the verification would report each as not promoted) and
// never looks up the turn of the delivery the host lost. The damage is on the attempts table: the confirmation's own
// write meets it, and the passes after the confirmation do not read it before they have something to report.
func TestHaltHostCheck_theNotesOfTheConfirmationsMadeBeforeAHaltReachTheTick(t *testing.T) {
	t.Parallel()
	host := &hostCheckHost{idleHost: &idleHost{}}
	d, s := keptAckDaemon(t, host, "ev-kept-1", "ev-kept-2", "ev-kept-3")
	seedEvent(t, s, "ev-905", delivery.Dispatched)
	// The first confirmation cannot write its attempt: an ordinary failure of that one item, not corruption.
	exec(t, s, "CREATE TRIGGER refuse_first BEFORE UPDATE ON attempts WHEN OLD.request_id = 'req-ev-kept-1' BEGIN SELECT RAISE(ABORT, 'refused'); END")
	// The reconciliation pass of the tick reads the requests before the confirmations do; the damage comes with the
	// read of the second request that the kept-acknowledgement confirmation makes.
	host.onRequest = func(id string) {
		if id == "req-ev-kept-2" && inConfirmation() {
			testsupport.DamageTable(t, s.DB, s.Path, "attempts")
		}
	}
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting confirmation returned %v instead of halting the store", err)
	}
	if !slices.Equal(host.confirmed, []string{"req-ev-kept-1", "req-ev-kept-2"}) {
		t.Fatalf("the confirmations reached are %v, want the first two only", host.confirmed)
	}
	if !slices.ContainsFunc(r.Notes, func(n string) bool { return strings.Contains(n, "kept acknowledgement ev-kept-1 not confirmed") }) {
		t.Fatalf("the note of the confirmation made before the halt was lost: %v", r.Notes)
	}
	for _, note := range r.Notes {
		if strings.HasPrefix(note, "acknowledgement ") || strings.Contains(note, "pending acknowledgement pass failed") {
			t.Fatalf("the acknowledgement verification was entered after the confirmation met the damage: %v", r.Notes)
		}
	}
	if r.TurnsLost != 0 || slices.Contains(host.turns, "dturn-ev-905") {
		t.Fatalf("the tick went on to the turn checks after the confirmation met the damage: lost %d, turn lookups %q", r.TurnsLost, host.turns)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM ack_evidence WHERE event_id = 'ev-kept-3' AND attempts = 1 AND next_check_at = ?", idleNow-98); got != 1 {
		t.Fatal("the confirmation behind the damaged one was touched")
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteWrite)
}

// Three delivered completions the host lost, in the order the recipient-turn check lists them. The first check settles
// its loss; the second meets the damage when the journal row of its loss is written; the third is never looked up.
func TestHaltHostCheck_aCorruptingTurnCheckHaltsTheTickBeforeTheDelivery(t *testing.T) {
	t.Parallel()
	host := &hostCheckHost{idleHost: &idleHost{}}
	d, s := idleDaemon(t, host.idleHost)
	d.Host = host
	exec(t, s, "UPDATE deliveries SET state='dispatched', attempt_count=1, next_eligible_at=NULL, dispatch_turn_id='dturn-"+idleEvent+"'")
	exec(t, s, "INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,record,sent_at,observed_at) VALUES('req-904',?,1,'completion_event','settled','dispatched','{\"turnId\":\"dturn-"+idleEvent+"\"}','2023-11-14T21:00:00+00:00','2023-11-14T21:00:00Z')", idleEvent)
	seedEvent(t, s, "ev-905", delivery.Dispatched)
	seedEvent(t, s, "ev-906", delivery.Dispatched)
	host.onTurn = func(turn string) {
		if turn == "dturn-ev-905" {
			testsupport.DamageTable(t, s.DB, s.Path, "journal")
		}
	}
	r, err := d.Tick(context.Background())
	if err != nil {
		t.Fatalf("a corrupting turn check returned %v instead of halting the store", err)
	}
	if r.TurnsLost != 1 {
		t.Fatalf("the loss settled before the halt was not reported: lost %d, notes %v", r.TurnsLost, r.Notes)
	}
	if !slices.Equal(host.turns, []string{"dturn-" + idleEvent, "dturn-ev-905"}) {
		t.Fatalf("the turns looked up are %v, want the first two only", host.turns)
	}
	if host.idleHost.holdTries != 0 || r.Delivered != 0 || r.Deferred != 0 {
		t.Fatalf("the tick went on after the halt: delivered %d, deferred %d, holds %d", r.Delivered, r.Deferred, host.idleHost.holdTries)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM deliveries WHERE event_id = 'ev-906' AND state = 'dispatched'"); got != 1 {
		t.Fatal("the delivery behind the damaged check was touched")
	}
	idleWakeAssertHaltMarker(t, d, store.HaltSiteWrite)
	idleWakeAssertNextTickWritesNothing(t, d, host.idleHost)
}
