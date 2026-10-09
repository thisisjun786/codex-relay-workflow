package faults

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945, evaluation of 901ee68a: the sweep's recording keeps the site of the statement that met the damage. The
// ledger reads a fault's row before it records another occurrence of it; a read of a damaged table is the
// observation site, and the recording must not replace that with the write site.
func TestRecordAllKeepsTheObservationSiteOfALedgerRead(t *testing.T) {
	l, ctx := obsLedger(t)
	first := Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: map[string]any{"turn": "t1"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}}
	if _, err := l.Record(ctx, first); err != nil {
		t.Fatal(err)
	}
	testsupport.DamageTable(t, l.Store.DB, l.Store.Path, "fault_ledger")
	second := first
	second.OccurrenceKey = "second"
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO}
	_, err := sw.RecordAll(ctx, l, Batch{Observations: []Observation{second}})
	if _, ok := store.CorruptingFailure(err); !ok {
		t.Fatalf("the fixture did not return a corrupt read: %v", err)
	}
	if site := store.SiteOf(err, ""); site != store.HaltSiteObservation {
		t.Fatalf("the ledger's read of the fault's row was marked %q, not the observation site", site)
	}
}

// A record whose statement is a write that meets the damage (the first record of a fault finds no row through
// the index and inserts into the damaged table) is the write site.
func TestRecordAllMarksALedgerInsertAtTheWriteSite(t *testing.T) {
	l, ctx := obsLedger(t)
	testsupport.DamageTable(t, l.Store.DB, l.Store.Path, "fault_ledger")
	o := Observation{Product: "crw", FaultClass: "report_omitted", Severity: Broken, Signature: map[string]any{"turn": "t1"}, OccurrenceKey: "first", Scope: map[string]any{"projectKey": "P"}}
	sw := &Sweeper{Store: l.Store, Now: l.Clock.ISO}
	_, err := sw.RecordAll(ctx, l, Batch{Observations: []Observation{o}})
	if _, ok := store.CorruptingFailure(err); !ok {
		t.Fatalf("the fixture did not return a corrupt failure: %v", err)
	}
	if site := store.SiteOf(err, ""); site != store.HaltSiteWrite {
		t.Fatalf("the ledger's insert was marked %q, not the write site", site)
	}
}
