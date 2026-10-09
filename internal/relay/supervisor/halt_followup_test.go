package supervisor

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

type countingOmission struct{ calls int }

func (o *countingOmission) Observe(ctx context.Context, r faults.ManagedReadingRequest) (any, error) {
	o.calls++
	return (OmissionObserver{}).Observe(ctx, r)
}

// CRW-945, verification round 1: with the real omission observer and two settled turns, the first turn that reads
// a damaged store publishes the marker once and the sweep reads no further turn.
func TestHaltFollowup_theRealObserverHaltEndsTheReadingOfTheNextTurn(t *testing.T) {
	c := newUnclaimedChild(t)
	c.exec(t, "INSERT INTO assignment_settlements VALUES('rel-1','child','continuation','completed',?)", nsAt)
	testsupport.DamageTable(t, c.s.DB, c.s.Path, "generation_turns")
	observer := &countingOmission{}
	sw := c.sweeperWith(observer)
	page, err := sw.ManagedReadings(c.ctx, c.r.Selection, observer, 8, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	state := store.HaltStateAt(c.s.Path)
	if !state.Present {
		t.Fatal("the real observer did not publish the marker")
	}
	if observer.calls != 1 || !page.Halted || state.Marker.Sequence != 1 {
		t.Fatalf("observer calls %d, halted %v, marker sequence %d", observer.calls, page.Halted, state.Marker.Sequence)
	}
}
