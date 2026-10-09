package supervisor

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945 items 2 and 3: the omission observer is the daemon's own observation of the store (CRW-848).
// The unclaimed-turn reading read the store through ReadOnlyRows and threw its corruption result away, so
// the halt helper saw the reading it started with; and the helper dropped the failure of the marker's
// publication, so the pass went on writing. The damage is a real SQLITE_CORRUPT (code 11) on one table of a
// temporary store.

// TestUnclaimedOmissionHalt_aDamagedStoreIsTheStoreUnreadableReading: the store read of the unclaimed
// reading meets the damage, and the reading says so in the shape the delivery reader uses for a store it
// cannot read, which is what the halt helper classifies: the observation marker is published.
func TestUnclaimedOmissionHalt_aDamagedStoreIsTheStoreUnreadableReading(t *testing.T) {
	c := newUnclaimedChild(t)
	testsupport.DamageTable(t, c.s.DB, c.s.Path, "generation_turns")
	reading := c.observe(t)
	reason, _ := reading["reason"].(string)
	if reading["reportingState"] != "unmeasured" || !strings.HasPrefix(reason, "store_unreadable: ") {
		t.Fatalf("the reading hides the damage of the store it read: %v", reading)
	}
	state := store.HaltStateAt(c.s.Path)
	if !state.Present || state.Detail != "" || state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteObservation {
		t.Fatalf("the damage left no observation marker: %+v", state)
	}
}

// TestUnclaimedOmissionHalt_aFailureThatIsNotCorruptionKeepsTheReading: a store read that fails for another
// reason leaves the delivery reader's answer and no marker, as before.
func TestUnclaimedOmissionHalt_aFailureThatIsNotCorruptionKeepsTheReading(t *testing.T) {
	c := newUnclaimedChild(t)
	c.exec(t, "DROP TABLE events")
	reading := c.observe(t)
	if reading["reportingState"] != "unmeasured" || reading["reason"] != "dispatch_uncorrelated" {
		t.Fatalf("a failure that is not corruption changed the reading: %v", reading)
	}
	if state := store.HaltStateAt(c.s.Path); state.Present {
		t.Fatalf("a failure that is not corruption published a marker: %+v", state)
	}
}

// TestHaltOnUnreadableStore_aMarkerThatCannotBePublishedIsReturned: the helper no longer drops the failure
// of RecordHalt. It returns an error the daemon classifies as the corrupting class, so the daemon keeps its
// own halt although the marker file could not be written.
func TestHaltOnUnreadableStore_aMarkerThatCannotBePublishedIsReturned(t *testing.T) {
	// Not parallel: the publication fault is process-wide.
	store.SetHaltFault(func(string) error { return errors.New("the state directory is full") })
	defer store.SetHaltFault(nil)
	selection := store.StateSelection{Path: t.TempDir()}
	reading := delivery.Obj{{Key: "reportingState", Value: "unmeasured"}, {Key: "reason", Value: "store_unreadable: disk I/O error (522)"}}
	err := haltOnUnreadableStore(t.Context(), selection, reading)
	if err == nil {
		t.Fatal("a marker that could not be published was dropped")
	}
	cause, ok := store.CorruptingFailure(err)
	if !ok || cause.Code != 522 {
		t.Fatalf("the failure is not classified as the corrupting class (code 522): %v, %+v", err, cause)
	}
	if !strings.Contains(err.Error(), "the state directory is full") {
		t.Fatalf("the error does not carry why the marker could not be written: %v", err)
	}
}

// TestHaltOnUnreadableStore_aPublishedMarkerReturnsNothing keeps the helper's answer for the case that
// already worked: the marker is written and the reading stands.
func TestHaltOnUnreadableStore_aPublishedMarkerReturnsNothing(t *testing.T) {
	t.Parallel()
	selection := store.StateSelection{Path: t.TempDir()}
	reading := delivery.Obj{{Key: "reportingState", Value: "unmeasured"}, {Key: "reason", Value: "store_unreadable: disk I/O error (522)"}}
	if err := haltOnUnreadableStore(t.Context(), selection, reading); err != nil {
		t.Fatal(err)
	}
	if state := store.HaltStateAt(selection.DBPath()); !state.Present {
		t.Fatal("no marker")
	}
}

// TestManagedReadingsHalt_theObserversCorruptionFailureEndsTheSweep: the fault sweep turned any observer
// error into a gap and read the next turn. An error of the corrupting class is the sweep's error, so the
// daemon halts on it; any other observer error stays a gap.
func TestManagedReadingsHalt_theObserversCorruptionFailureEndsTheSweep(t *testing.T) {
	// Not parallel: the publication fault is process-wide.
	c := newUnclaimedChild(t)
	testsupport.DamageTable(t, c.s.DB, c.s.Path, "generation_turns")
	store.SetHaltFault(func(string) error { return errors.New("the state directory is full") })
	defer store.SetHaltFault(nil)
	sw := &faults.Sweeper{Store: c.s, Selection: c.r.Selection, ManagedObserver: OmissionObserver{}, Now: c.clock.ISO, HostRecordPath: c.root + "/absent-host-record"}
	_, err := sw.Sweep(c.ctx, "crw")
	if err == nil {
		t.Fatal("the sweep went on after the observer could neither read the store nor publish the marker")
	}
	if _, ok := store.CorruptingFailure(err); !ok {
		t.Fatalf("the sweep's error is not the corrupting class: %v", err)
	}
}

type failingObserver struct{ err error }

func (o failingObserver) Observe(_ context.Context, _ faults.ManagedReadingRequest) (any, error) {
	return nil, o.err
}

func (c *unclaimedChild) sweeperWith(observer faults.ManagedReadingObserver) *faults.Sweeper {
	return &faults.Sweeper{Store: c.s, Selection: c.r.Selection, ManagedObserver: observer, Now: c.clock.ISO, HostRecordPath: c.root + "/absent-host-record"}
}

func TestManagedReadingsHalt_anObserverFailureThatIsNotCorruptionStaysAGap(t *testing.T) {
	t.Parallel()
	c := newUnclaimedChild(t)
	page, err := c.sweeperWith(failingObserver{errors.New("the observer is down")}).ManagedReadings(c.ctx, c.r.Selection, failingObserver{errors.New("the observer is down")}, 8, nil, "")
	if err != nil {
		t.Fatalf("an error that is not corruption ended the sweep: %v", err)
	}
	if len(page.Gaps) != 1 {
		t.Fatalf("gaps %v", page.Gaps)
	}
}

// claimedReadyForReview turns the unclaimed child into a claimed one whose turn declared ready_for_review, so the
// observer reads the receipt of the turn from the store after it has read the turn's registry context.
func (c *unclaimedChild) claimedReadyForReview(t *testing.T) {
	t.Helper()
	c.marker(t, "claims/child/claim.json", map[string]any{"sessionId": "child", "dispatchRequestId": "dispatch-1"})
	c.marker(t, "dispositions/child/business.json", map[string]any{"outcome": "ready_for_review", "sessionId": "child", "turnId": "business"})
	// A reviewable head with its lineage row, so the head lookup reads the lineage table.
	c.event(t, "child", "final", "ready_for_review", 1)
	c.exec(t, "INSERT INTO revision_lineage(relationship_id,execution_generation,event_id,revision_hash,supersedes_hash,declared_by,recorded_at) VALUES('rel-1',1,'receipt','x',NULL,'child',?)", nsAt)
}

// CRW-945 (evaluation of 901ee68a): the receipt lookup of a claimed ready_for_review turn is a read of the store
// by the daemon's own observation. Its failure of the corrupting class reached the omission reading as the
// receipt_unreadable of any other failure, so no marker was published and the sweep went on to record.
func TestOmissionHalt_aDamagedReceiptReadIsTheStoreUnreadableReading(t *testing.T) {
	c := newUnclaimedChild(t)
	c.claimedReadyForReview(t)
	healthy := c.observe(t)
	if current, _ := healthy["currentObservation"].(map[string]any); current["label"] == nil {
		t.Fatalf("the fixture is not a claimed turn whose receipt the observer reads: %v", healthy)
	}
	testsupport.DamageTable(t, c.s.DB, c.s.Path, "revision_lineage")
	reading := c.observe(t)
	reason, _ := reading["reason"].(string)
	if reading["reportingState"] != "unmeasured" || !strings.HasPrefix(reason, "store_unreadable: ") {
		t.Fatalf("the reading hides the damage of the receipt read: %v", reading)
	}
	state := store.HaltStateAt(c.s.Path)
	if !state.Present || state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteObservation {
		t.Fatalf("the damage left no observation marker: %+v", state)
	}
}

// A receipt read that fails for another reason is the receipt_unreadable it always was, with no marker.
func TestOmissionHalt_aReceiptReadThatIsNotCorruptionStaysUnreadable(t *testing.T) {
	c := newUnclaimedChild(t)
	c.claimedReadyForReview(t)
	c.exec(t, "DROP TABLE revision_lineage")
	reading := c.observe(t)
	if reading["reportingState"] != "unmeasured" || reading["reason"] != "receipt_unreadable" {
		t.Fatalf("a failure that is not corruption changed the reading: %v", reading)
	}
	if state := store.HaltStateAt(c.s.Path); state.Present {
		t.Fatalf("a failure that is not corruption published a marker: %+v", state)
	}
}

// Only the observer asks for the corruption: the guard's answer for an unreadable store (the Stop hook's) stays
// (nil, false, nil).
func TestOmissionHalt_theGuardsAnswerForAnUnreadableStoreIsUnchanged(t *testing.T) {
	c := newUnclaimedChild(t)
	c.claimedReadyForReview(t)
	testsupport.DamageTable(t, c.s.DB, c.s.Path, "revision_lineage")
	want := delivery.ReceiptQuery{Relationship: "rel-1", Session: "child", Turn: "business", Generation: int64(1), Dispatch: "dispatch-1"}
	receipt, readable, err := delivery.LookupStoredReceiptAt(c.ctx, c.s.Path, nil, time.Second, want)
	if receipt != nil || readable || err != nil {
		t.Fatalf("the guard's answer moved: %v %v %v", receipt, readable, err)
	}
	want.ReportCorruption = true
	_, readable, err = delivery.LookupStoredReceiptAt(c.ctx, c.s.Path, nil, time.Second, want)
	cause, corrupting := store.CorruptingFailure(err)
	if readable || !corrupting || cause.Code != 11 || store.SiteOf(err, "") != store.HaltSiteObservation {
		t.Fatalf("the observer's lookup did not report the corruption at the observation site: readable %v, %v", readable, err)
	}
}
