package adapter

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-397: the fault sweep the daemon runs closes a delivery_stalled fault when the delivery it was opened
// for is acknowledged, or is overtaken, and does not open it again on the next pass. The sweep is the one
// daemonFactory builds, over a store in a temporary directory, as in daemon_sweep_test.go.

// ackWorld is the daemon's sweeper over a store holding one delivery whose first two attempts were withheld
// before sending, in the shape of the operating store's: the relationship is at generation 1, and the
// delivery's event is the head of its generation, so nothing but its own state decides whether it is stalled.
type ackWorld struct {
	t       *testing.T
	ctx     context.Context
	sweeper *faults.Sweeper
	ledger  *faults.Ledger
	store   *store.Store
}

func newAckWorld(t *testing.T) *ackWorld {
	t.Helper()
	ctx := context.Background()
	// The variant that gives the managed turn its own receipt, so that the sweep files no omission beside the delivery fault.
	sweeper, ledger, s := daemonSweepWorld(t, ctx, "own_receipt")
	w := &ackWorld{t: t, ctx: ctx, sweeper: sweeper, ledger: ledger, store: s}
	w.exec("INSERT INTO relationships(relationship_id,issue_key,status,parent_task_id,parent_host_id,child_task_id,child_host_id,child_cwd,execution_generation,artifact_roots,allowed_recipients,created_at,updated_at) VALUES('rel-d','ISSUE-D','active','parent','host','child-d','host','/work',1,'[]','[]','stamp','stamp')")
	w.exec("INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES('ev-d','rel-d',1,'x','ready_for_review','child','child-d','turn-d','completed','{}','final','stamp','stamp')")
	w.exec("INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,hold_reason,created_at,updated_at) VALUES('ev-d','rel-d','completion_event','parent','thread','withheld_pre_send',2,NULL,'stamp','stamp')")
	w.attempt(1, "withheld_pre_send")
	w.attempt(2, "withheld_pre_send")
	return w
}

func (w *ackWorld) exec(query string, args ...any) {
	w.t.Helper()
	if _, err := w.store.Q(w.ctx).ExecContext(w.ctx, query, args...); err != nil {
		w.t.Fatalf("%s: %v", query, err)
	}
}

func (w *ackWorld) attempt(number int, state string) {
	w.t.Helper()
	w.exec("INSERT INTO attempts(request_id,event_id,attempt_no,kind,internal_state,state,observed_at) VALUES(?,'ev-d',?,'completion_event','settled',?,'stamp')", "del-d-a"+string(rune('0'+number)), number, state)
}

// dispatch is the third attempt reaching the recipient; acknowledge is the recipient's verified acknowledgement.
func (w *ackWorld) dispatch() {
	w.t.Helper()
	w.attempt(3, "dispatched")
	w.exec("UPDATE deliveries SET state='dispatched', attempt_count=3 WHERE event_id='ev-d'")
}

func (w *ackWorld) acknowledge() {
	w.t.Helper()
	w.exec("INSERT INTO acks(event_id,record,ack_turn_id,accepted,verified,ack_at) VALUES('ev-d','{}','turn-p',1,'verified','stamp')")
	w.exec("UPDATE deliveries SET state='acknowledged' WHERE event_id='ev-d'")
}

// pass is one daemon sweep: the sweep itself, then recording what it found.
func (w *ackWorld) pass() {
	w.t.Helper()
	batch, err := w.sweeper.Sweep(w.ctx, "crw")
	if err != nil {
		w.t.Fatal(err)
	}
	if _, err = w.sweeper.RecordAll(w.ctx, w.ledger, batch); err != nil {
		w.t.Fatal(err)
	}
}

// standing is how many delivery_stalled faults have not been closed.
func (w *ackWorld) standing() int64 {
	w.t.Helper()
	row, err := w.store.One(w.ctx, "SELECT COUNT(*) AS n FROM fault_ledger WHERE fault_class='delivery_stalled' AND state NOT IN ('resolved','withdrawn')")
	if err != nil {
		w.t.Fatal(err)
	}
	n, _ := row.Get("n").(int64)
	return n
}

func (w *ackWorld) wantStanding(want int64, when string) {
	w.t.Helper()
	if got := w.standing(); got != want {
		w.t.Fatalf("%s: %d delivery_stalled faults stand, want %d", when, got, want)
	}
}

func TestDaemonSweepClearsAStalledDeliveryFaultOnceItIsAcknowledged(t *testing.T) {
	t.Run("acknowledged after the attempts that failed", func(t *testing.T) {
		w := newAckWorld(t)
		w.pass()
		w.wantStanding(1, "while the delivery is withheld")
		w.dispatch()
		w.acknowledge()
		w.pass()
		w.wantStanding(0, "once the delivery is acknowledged")
		w.pass()
		w.wantStanding(0, "on the pass after it was closed")
	})
	t.Run("dispatched, then acknowledged", func(t *testing.T) {
		w := newAckWorld(t)
		w.pass()
		w.wantStanding(1, "while the delivery is withheld")
		w.dispatch()
		w.pass()
		w.wantStanding(0, "once the delivery is dispatched")
		w.acknowledge()
		w.pass()
		w.wantStanding(0, "once the delivery is acknowledged, which does not open the fault again")
	})
}

// A delivery whose event the relationship has moved past is no longer something to deliver. The CLI sweep
// closes its fault; the daemon sweep must agree, and not open it again from the attempts it left behind.
func TestDaemonSweepDoesNotOpenAnOvertakenDeliveryFaultAgain(t *testing.T) {
	w := newAckWorld(t)
	w.pass()
	w.wantStanding(1, "while the delivery is withheld")
	w.exec("UPDATE relationships SET execution_generation=2 WHERE relationship_id='rel-d'")
	w.pass()
	w.wantStanding(0, "once the relationship is past the delivery's generation")
	w.pass()
	w.wantStanding(0, "on the pass after it was closed")
}
