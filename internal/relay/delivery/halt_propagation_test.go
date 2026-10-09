package delivery

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945 item 1: the daemon's halt branch (CRW-848) can only see a corrupting failure that a sub-pass hands
// back. The reconciliation pass and the delivery scheduler turned a per-attempt failure into a note and went on;
// a failure of the corrupting class has to end the pass and reach the caller, and every other failure stays the
// note it has been. The damage is real SQLITE_CORRUPT (code 11) on one table, so the rest of the store still
// answers, which is the state in which the swallowed error left the pass writing on.

// reconcileWorld is a fixture with one held attempt a reconciliation can act on.
func reconcileWorld(t *testing.T) (*fixture, *Reconciler, string) {
	t.Helper()
	f := newFixture(t, "")
	event := f.queuedEvent(regOpts{})
	f.host.script = []string{"in_progress"}
	request := pyjson.Text(f.mustAttempt(event, nil).Get("requestId"))
	f.host.ledger[request] = Obj{{Key: "status", Value: Accepted}}
	f.clock.Advance(1000)
	return f, NewReconciler(f.delivery), request
}

func requireCorruption(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("the pass returned no error")
	}
	if cause, ok := store.CorruptingFailure(err); !ok || cause.Code != 11 {
		t.Fatalf("the pass returned %v, which is not the corrupting class (code 11): %+v", err, cause)
	}
}

func TestReconcilePassHalt_aCorruptingAttemptFailureEndsThePassAndIsReturned(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"journal", "events"} {
		t.Run(table, func(t *testing.T) {
			f, rc, request := reconcileWorld(t)
			testsupport.DamageTable(t, f.store.DB, f.store.Path, table)
			var report ReconcileReport
			err := ReconcilePass(f.ctx, rc, f.host, 8, f.clock.Now(), &report)
			requireCorruption(t, err)
			if report.Reconciled != 0 {
				t.Fatalf("a pass that met the damage went on to count a reconciliation: %+v", report)
			}
			// the failure is not recorded as the attempt's own: the pass wrote no gate row about it
			if gate := f.one("SELECT COUNT(*) AS c FROM reconcile_gate WHERE request_id = ? AND retry_required = 1 AND last_error LIKE '%malformed%'", request).I("c"); gate != 0 {
				t.Fatalf("the corrupting failure was stored as the attempt's retry reason (%d rows)", gate)
			}
		})
	}
}

func TestReconcilePassHalt_aFailureThatIsNotCorruptionStaysANoteAndTheGateRow(t *testing.T) {
	t.Parallel()
	f, rc, request := reconcileWorld(t)
	if _, err := f.store.DB.Exec("DROP TABLE journal"); err != nil {
		t.Fatal(err)
	}
	var report ReconcileReport
	if err := ReconcilePass(f.ctx, rc, f.host, 8, f.clock.Now(), &report); err != nil {
		t.Fatalf("a failure that is not corruption ended the pass: %v", err)
	}
	if len(report.Notes) != 1 || !strings.Contains(report.Notes[0], "reconcile failed for "+request) {
		t.Fatalf("notes %v", report.Notes)
	}
	if got := f.one("SELECT retry_required AS c FROM reconcile_gate WHERE request_id = ?", request).I("c"); got != 1 {
		t.Fatalf("the failure was not stored as a retry (retry_required %d)", got)
	}
}

// schedulerWorld is a fixture with one delivery due and a host that accepts it.
func schedulerWorld(t *testing.T) (*fixture, *Scheduler) {
	t.Helper()
	f := newFixture(t, "")
	f.queuedEvent(regOpts{})
	f.host.script = []string{"sent"}
	return f, &Scheduler{Delivery: f.delivery, MaxSendsTick: 4}
}

func TestSchedulerHalt_aCorruptingAttemptFailureEndsThePassAndIsReturned(t *testing.T) {
	t.Parallel()
	for _, table := range []string{"journal", "attempts", "generations"} {
		t.Run(table, func(t *testing.T) {
			f, sc := schedulerWorld(t)
			testsupport.DamageTable(t, f.store.DB, f.store.Path, table)
			var counts TickCounts
			err := sc.Deliver(f.ctx, f.host, f.clock.Now(), &counts)
			requireCorruption(t, err)
			if counts.Delivered != 0 {
				t.Fatalf("a delivery counted after the damage: %+v", counts)
			}
		})
	}
}

func TestSchedulerHalt_aFailureThatIsNotCorruptionStaysANote(t *testing.T) {
	t.Parallel()
	f, sc := schedulerWorld(t)
	if _, err := f.store.DB.Exec("DROP TABLE journal"); err != nil {
		t.Fatal(err)
	}
	var counts TickCounts
	if err := sc.Deliver(f.ctx, f.host, f.clock.Now(), &counts); err != nil {
		t.Fatalf("a failure that is not corruption ended the pass: %v", err)
	}
	if len(counts.Notes) != 1 || !strings.Contains(counts.Notes[0], "delivery refused for") {
		t.Fatalf("notes %v", counts.Notes)
	}
}

// CRW-945 item 5: a corrupting failure the pass met in its own read of the store carries the observation site, and
// one it met in a statement that changes the store carries the write site, so the daemon's marker says which.
func requireSite(t *testing.T, err error, want string) {
	t.Helper()
	requireCorruption(t, err)
	if got := store.SiteOf(err, "unmarked"); got != want {
		t.Fatalf("the failure carries site %q, want %q: %v", got, want, err)
	}
}

func TestReconcilePassHalt_aReadFailureIsMarkedAtTheObservationSite(t *testing.T) {
	t.Parallel()
	f, rc, _ := reconcileWorld(t)
	testsupport.DamageTable(t, f.store.DB, f.store.Path, "attempts")
	var report ReconcileReport
	requireSite(t, ReconcilePass(f.ctx, rc, f.host, 8, f.clock.Now(), &report), store.HaltSiteObservation)
}

// The gate row the pass stores after an attempt is its write: the same table damaged, met by the statement that
// changes it, is the write site.
func TestReconcilePassHalt_aGateWriteFailureIsMarkedAtTheWriteSite(t *testing.T) {
	t.Parallel()
	f, rc, _ := reconcileWorld(t)
	testsupport.DamageTable(t, f.store.DB, f.store.Path, "reconcile_gate")
	var report ReconcileReport
	requireSite(t, ReconcilePass(f.ctx, rc, f.host, 8, f.clock.Now(), &report), store.HaltSiteWrite)
}

func TestSchedulerHalt_aListingReadFailureIsMarkedAtTheObservationSite(t *testing.T) {
	t.Parallel()
	f, sc := schedulerWorld(t)
	testsupport.DamageTable(t, f.store.DB, f.store.Path, "events")
	var counts TickCounts
	requireSite(t, sc.Deliver(f.ctx, f.host, f.clock.Now(), &counts), store.HaltSiteObservation)
}
