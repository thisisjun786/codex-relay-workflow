package daemon

import (
	"context"
	"errors"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945, verification round 2: a corrupt read of the observation pass halts the store before the ends the pass
// held back are settled; a marker the observer published ends the page whatever the observer then answers; and a
// delivery pass whose follow-up read fails for another reason still reports what it did.

// failingPublishingObserver publishes the halt marker and then answers with a failure that is not corruption, or with
// no reading at all.
type failingPublishingObserver struct {
	s     *store.Store
	empty bool
	calls int
}

func (o *failingPublishingObserver) Observe(ctx context.Context, _ faults.ManagedReadingRequest) (any, error) {
	o.calls++
	cause := store.CorruptingCause{Code: 11, Message: "database disk image is malformed", Site: store.HaltSiteObservation}
	if err := store.RecordHalt(ctx, o.s.Path, cause); err != nil {
		return nil, err
	}
	if o.empty {
		return nil, nil
	}
	return nil, errors.New("observer failed after publishing the marker")
}

func TestHaltRound2_aPublishedMarkerEndsThePageWhateverTheObserverAnswers(t *testing.T) {
	for _, empty := range []bool{false, true} {
		name := "failure"
		if empty {
			name = "no reading"
		}
		t.Run(name, func(t *testing.T) {
			d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
			attachManagedSettlement(t, s)
			exec(t, s, "INSERT INTO assignment_settlements VALUES('r','child','continuation','completed','2023-11-14T22:13:21Z')")
			observer := &failingPublishingObserver{s: s, empty: empty}
			sw := &faults.Sweeper{Store: s, Now: d.Clock.ISO}
			page, err := sw.ManagedReadings(ctx, nil, observer, 8, nil, "")
			if err != nil {
				t.Fatal(err)
			}
			if observer.calls != 1 || !page.Halted || len(page.Gaps) != 0 {
				t.Fatalf("calls %d halted %v gaps %v: a turn was read or gapped after the marker stood", observer.calls, page.Halted, page.Gaps)
			}
			if seq := store.HaltStateAt(s.Path).Marker.Sequence; seq != 1 {
				t.Fatalf("the marker was published %d times", seq)
			}
		})
	}
}

// A marker that already stands when the page begins is the halt: no turn is read on it.
func TestHaltRound2_aMarkerStandingBeforeThePageReadsNoTurn(t *testing.T) {
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	attachManagedSettlement(t, s)
	if err := store.RecordHalt(ctx, s.Path, store.CorruptingCause{Code: 11, Message: "database disk image is malformed", Site: store.HaltSiteWrite}); err != nil {
		t.Fatal(err)
	}
	observer := &failingPublishingObserver{s: s}
	sw := &faults.Sweeper{Store: s, Now: d.Clock.ISO}
	page, err := sw.ManagedReadings(ctx, nil, observer, 8, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if observer.calls != 0 || !page.Halted {
		t.Fatalf("calls %d halted %v: a turn was read on a halted store", observer.calls, page.Halted)
	}
}

// The first turn of the pass ended interrupted and waits for the pass to end; the second turn's read of the store
// meets real damage. The halt stands before the deferred end would be settled, so no settlement is attempted, and
// the marker names the read that met the damage.
func TestHaltRound2_aCorruptReadOfThePassSettlesNoDeferredEnd(t *testing.T) {
	h := &observationHost{status: "interrupted"}
	d, s, ctx := haltCoverageDaemon(t, h)
	d.Policy.MaxTurnReads = 3
	h.onRead = func() {
		if len(h.reads) == 2 {
			testsupport.DamageIndex(t, s.DB, s.Path, "sqlite_autoindex_assignment_settlements_1")
		}
	}
	settlements := 0
	d.beforeSettle = func(store.TurnReference) { settlements++ }
	r := Report{}
	err := d.observe(ctx, &r)
	if _, ok := store.CorruptingFailure(err); !ok {
		t.Fatalf("the fixture did not return a corrupt read: %v, report %+v", err, r)
	}
	if len(h.reads) != 2 {
		t.Fatalf("the pass went on after the damage: reads %v", h.reads)
	}
	if settlements != 0 {
		t.Fatalf("%d settlements were attempted after the corrupt read: notes %v", settlements, r.Notes)
	}
	state := store.HaltStateAt(s.Path)
	if !d.haltedStore || !state.Present || state.Marker.Site != store.HaltSiteObservation {
		t.Fatalf("halted %v, marker %+v: the corrupt read was not halted at the observation site", d.haltedStore, state.Marker)
	}
}

// The delivery pass deferred a delivery, and the read of the waiting heads after it failed for a reason that is
// not corruption: the tick's error still comes with the deferral the pass made.
func TestHaltRound2_aFailedHoldReadKeepsTheDeliveryCounts(t *testing.T) {
	h := &idleHost{}
	d, s := idleDaemon(t, h)
	exec(t, s, "UPDATE deliveries SET next_eligible_at=NULL")
	d.idle.beforeHold = func() { exec(t, s, "DROP TABLE deliveries") }
	r, err := d.Tick(context.Background())
	if err == nil {
		t.Fatal("the fixture did not fail the read after the delivery pass")
	}
	if r.Deferred != 1 {
		t.Fatalf("the deferral the delivery pass made is missing: deferred %d, error %v, notes %v", r.Deferred, err, r.Notes)
	}
}
