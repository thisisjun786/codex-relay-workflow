package daemon

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945, verification round 1: the halt a pass publishes ends the pass; what the pass did before the halt is
// still the tick's; and a failure of a read is marked at the observation site.

// publishingObserver is an observer that publishes the halt marker the way the omission observer does when it
// reads a damaged store, and answers with the reading of an unmeasured turn.
type publishingObserver struct {
	s *store.Store
	// failRecovery drops a table the sweep reads after the readings, so the sweep returns a failure that is not
	// corruption once the marker stands.
	failRecovery bool
	calls        int
}

func (o *publishingObserver) Observe(ctx context.Context, _ faults.ManagedReadingRequest) (any, error) {
	o.calls++
	cause := store.CorruptingCause{Code: 11, Message: "database disk image is malformed", Site: store.HaltSiteObservation}
	if err := store.RecordHalt(ctx, o.s.Path, cause); err != nil {
		return nil, err
	}
	if o.failRecovery {
		if _, err := o.s.DB.Exec("DROP TABLE fault_ledger"); err != nil {
			return nil, err
		}
	}
	return map[string]any{"reportingState": "unmeasured", "reason": "store_unreadable: database disk image is malformed (11)", "relationshipId": "r"}, nil
}

func TestHaltFollowup_aPublishedMarkerStopsTheNextManagedReading(t *testing.T) {
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	attachManagedSettlement(t, s)
	exec(t, s, "INSERT INTO assignment_settlements VALUES('r','child','continuation','completed','2023-11-14T22:13:21Z')")
	observer := &publishingObserver{s: s}
	sw := &faults.Sweeper{Store: s, Now: d.Clock.ISO}
	page, err := sw.ManagedReadings(ctx, store.StateSelection{Path: filepath.Dir(s.Path)}, observer, 8, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if observer.calls != 1 || !page.Halted || len(page.Gaps) != 0 {
		t.Fatalf("calls %d halted %v gaps %v: a turn was read after the marker stood", observer.calls, page.Halted, page.Gaps)
	}
	if seq := store.HaltStateAt(s.Path).Marker.Sequence; seq != 1 {
		t.Fatalf("the marker was published %d times", seq)
	}
}

func TestHaltFollowup_aPublishedMarkerSurvivesALaterSweepFailure(t *testing.T) {
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	attachManagedSettlement(t, s)
	observer := &publishingObserver{s: s, failRecovery: true}
	d.Faults = &faults.Ledger{Store: s, Clock: d.Clock}
	d.Sweeper = &faults.Sweeper{Store: s, Selection: store.StateSelection{Path: filepath.Dir(s.Path)}, ManagedObserver: observer, Now: d.Clock.ISO, HostRecordPath: filepath.Join(filepath.Dir(s.Path), "absent-host-record")}
	r := Report{}
	if err := d.sweep(ctx, &r); err != nil {
		t.Fatal(err)
	}
	if !store.HaltStateAt(s.Path).Present {
		t.Fatal("the fixture did not publish the marker")
	}
	if !d.haltedStore {
		t.Fatalf("the published marker was not adopted: %v", r.Notes)
	}
}

// A listing of the delivery pass that cannot be read is the relay's read of the store, not a write.
func TestHaltFollowup_aDeliveryListingReadIsMarkedAtTheObservationSite(t *testing.T) {
	h := &idleHost{}
	d, s := idleDaemon(t, h)
	exec(t, s, "UPDATE deliveries SET next_eligible_at=NULL")
	d.idle.beforeIdle = func() { testsupport.DamageTable(t, s.DB, s.Path, "events") }
	if _, err := d.Tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	state := store.HaltStateAt(s.Path)
	if !state.Present {
		t.Fatal("the read failure did not halt the store")
	}
	if state.Marker.Site != store.HaltSiteObservation {
		t.Fatalf("the delivery listing was marked at %q", state.Marker.Site)
	}
}

// damagingHost damages a table on its second read of a thread, i.e. in the second delivery attempt of the pass.
type damagingHost struct {
	*idleHost
	s     *store.Store
	t     *testing.T
	reads int
}

func (h *damagingHost) ReadThread(ctx context.Context, thread string) (delivery.ThreadFacts, error) {
	h.reads++
	if h.reads == 2 {
		testsupport.DamageTable(h.t, h.s.DB, h.s.Path, "journal")
	}
	return h.idleHost.ReadThread(ctx, thread)
}

// The first attempt of the pass deferred its delivery (the recipient is busy) and the second met the damage: the
// halt's report still says what the first did.
func TestHaltFollowup_aHaltInTheDeliveryPassKeepsTheEarlierAttempts(t *testing.T) {
	h := &damagingHost{idleHost: &idleHost{}, t: t}
	ctx := context.Background()
	s, err := fixtureStore(ctx, filepath.Join(t.TempDir(), "relay.sqlite3"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h.s = s
	seedWaitingHead(t, s, idleNow)
	seed(t, s, "rel-905", "parent-905", "child-905", "anchor-905")
	exec(t, s, "INSERT INTO events(event_id,relationship_id,execution_generation,revision_hash,outcome,producer,turn_thread_id,turn_id,turn_status,receipt,stage,first_seen_at,last_seen_at) VALUES('ev-905','rel-905',1,'rev-905','ready_for_review','child','child-905','turn-905','completed','{}','final','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	exec(t, s, "INSERT INTO deliveries(event_id,relationship_id,kind,recipient_task_id,recipient_thread_id,state,attempt_count,created_at,updated_at) VALUES('ev-905','rel-905','completion_event','parent-905','parent-905','queued',0,'2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	exec(t, s, "UPDATE deliveries SET next_eligible_at=NULL")
	d := New(s, h, &delivery.FakeClock{T: idleNow}, nil)
	r, err := d.Tick(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !d.haltedStore || h.reads != 2 {
		t.Fatalf("the fixture did not halt on the second attempt: halted %v, reads %d", d.haltedStore, h.reads)
	}
	if got := count(t, s, "SELECT COUNT(*) FROM deliveries WHERE state='deferred_busy'"); got != 1 {
		t.Fatalf("the earlier deferral is not in the store: %d", got)
	}
	if r.Deferred != 1 {
		t.Fatalf("the earlier deferral is missing from the halt's report: %+v", r)
	}
}
