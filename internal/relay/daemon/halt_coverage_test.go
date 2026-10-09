package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/testsupport"
)

// CRW-945 (items 3, 4 and 5; items 1 and 2 are pinned in delivery and supervisor): a failure of the corrupting
// class (CRW-848) has to reach the daemon's halt, and once the halt stands in a pass nothing more is read or
// written by it. The damage is a real SQLITE_CORRUPT (code 11) on one table of a temporary store, so the rest of
// the store still answers, which is the state in which these paths kept writing.

func haltCoverageDaemon(t *testing.T, host *observationHost) (*Daemon, *store.Store, context.Context) {
	t.Helper()
	ctx, s := lateStore(t)
	d := New(s, host, &delivery.FakeClock{T: 1700000000}, nil)
	d.Policy.MaxTurnReads, d.Policy.MaxSends = 3, -1
	return d, s, ctx
}

// Item 4: a settlement that halts the store ends the observation loop and the deferred settlements on the
// spot. The pass read the host for the next turn, wrote its poll row and settled the ends it had deferred,
// all against a store it had just seen damaged, and every later step of the tick ran as well.
func TestHaltCoverage_aSettlementThatHaltsEndsTheObservationPass(t *testing.T) {
	t.Parallel()
	for name, statuses := range map[string]map[string]string{
		"every turn completed":                    nil,
		"the turns that failed wait for the pass": {"business": "interrupted", "continuation": "failed"},
	} {
		t.Run(name, func(t *testing.T) {
			host := &observationHost{status: "completed", statuses: statuses}
			d, s, ctx := haltCoverageDaemon(t, host)
			settlements := 0
			d.beforeSettle = func(store.TurnReference) {
				settlements++
				if settlements == 1 {
					testsupport.DamageTable(t, s.DB, s.Path, "observations")
				}
			}
			r, err := d.Tick(ctx)
			if err != nil {
				t.Fatalf("a settlement that met the damage returned %v instead of halting the store", err)
			}
			if !d.haltedStore {
				t.Fatal("the settlement's failure did not keep the process's halt")
			}
			state := store.HaltStateAt(s.Path)
			if !state.Present || state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteWrite {
				t.Fatalf("marker %+v", state)
			}
			if settlements != 1 {
				t.Fatalf("%d settlements were attempted after the first one halted the store", settlements-1)
			}
			if len(host.reads) > 1 {
				t.Fatalf("the pass read the host %d more times after the store was halted: %v", len(host.reads)-1, host.reads)
			}
			if polls := count(t, s, "SELECT COUNT(*) FROM poll_observations"); polls > 1 {
				t.Fatalf("the pass wrote %d more poll rows after the store was halted", polls-1)
			}
			if r.Observed != 0 {
				t.Fatalf("a settlement that failed was counted: %+v", r)
			}
		})
	}
}

// Item 3: a corrupting failure the omission observer returns when it cannot publish the marker reaches the
// daemon's classifier, so the process keeps its own halt although no marker file exists.
func TestHaltCoverage_aMarkerTheObserverCouldNotPublishStillHaltsTheProcess(t *testing.T) {
	// Not parallel: it installs the store package's publication fault, which is process-wide.
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	attachManagedSettlement(t, s)
	d.Faults = &faults.Ledger{Store: s, Clock: d.Clock}
	d.Sweeper = &faults.Sweeper{Store: s, Selection: store.StateSelection{Path: filepath.Dir(s.Path)}, ManagedObserver: stubObserver{err: &store.DetectedCorruption{Cause: store.CorruptingCause{Code: 522, Message: "disk I/O error", Site: store.HaltSiteObservation}, Err: errors.New("store writes are halted, and the halt marker could not be written: the state directory is full")}}, Now: d.Clock.ISO, HostRecordPath: filepath.Join(filepath.Dir(s.Path), "absent-host-record")}
	store.SetHaltFault(func(string) error { return errors.New("the state directory is full") })
	defer store.SetHaltFault(nil)
	r, err := d.Tick(ctx)
	if err != nil {
		t.Fatalf("the observer's corrupting failure returned %v instead of halting the store", err)
	}
	if !d.haltedStore {
		t.Fatalf("the process kept no halt: %+v", r.Notes)
	}
	if state := store.HaltStateAt(s.Path); state.Present {
		t.Fatalf("a marker was published through the fault: %+v", state)
	}
	if cursors := count(t, s, "SELECT COUNT(*) FROM fault_cursors"); cursors != 0 {
		t.Fatalf("the sweep recorded %d cursors after the observer's failure", cursors)
	}
}

// Item 3, the other half: an observer that did publish the marker ends the sweep too. Its reading is not an
// error, so the daemon looks at the marker between the sweep's reading and its recording.
func TestHaltCoverage_aMarkerTheObserverPublishedEndsTheSweepBeforeItRecords(t *testing.T) {
	t.Parallel()
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	attachManagedSettlement(t, s)
	d.Faults = &faults.Ledger{Store: s, Clock: d.Clock}
	var once sync.Once
	published := func() {
		once.Do(func() {
			if err := store.RecordHalt(ctx, s.Path, store.CorruptingCause{Code: 522, Message: "disk I/O error", Site: store.HaltSiteObservation}); err != nil {
				t.Error(err)
			}
		})
	}
	d.Sweeper = &faults.Sweeper{Store: s, Selection: store.StateSelection{Path: filepath.Dir(s.Path)}, ManagedObserver: stubObserver{reading: map[string]any{"reportingState": "unmeasured", "reason": "store_unreadable: disk I/O error (522)", "relationshipId": "r"}, before: published}, Now: d.Clock.ISO, HostRecordPath: filepath.Join(filepath.Dir(s.Path), "absent-host-record")}
	if _, err := d.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if !d.haltedStore {
		t.Fatal("the process did not take over the marker the observer published")
	}
	if cursors := count(t, s, "SELECT COUNT(*) FROM fault_cursors"); cursors != 0 {
		t.Fatalf("the sweep recorded %d cursors after the observer published the halt", cursors)
	}
	if again := store.HaltStateAt(s.Path); again.Marker.Sequence != 1 {
		t.Fatalf("a second marker was published for the same damage: %+v", again.Marker)
	}
}

// Item 5: the requeue's write is a write. A requeue whose Enqueue meets the damage marks the write site, and the
// scan that reads the intents marks the observation site, as the other steps of the tick do.
func TestHaltCoverage_aRequeueWriteIsMarkedAtTheWriteSite(t *testing.T) {
	t.Parallel()
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	ownReceipt(t, s, "business", "ready_for_review", "final", "", owedIntent)
	exec(t, s, "UPDATE delivery_intent SET kind='completion_event', next_retry_at=NULL")
	testsupport.DamageTable(t, s.DB, s.Path, "journal")
	if _, err := d.Tick(ctx); err != nil {
		t.Fatalf("a requeue write that met the damage returned %v instead of halting the store", err)
	}
	state := store.HaltStateAt(s.Path)
	if !state.Present || state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteWrite {
		t.Fatalf("the requeue's write was not marked at the write site: %+v", state)
	}
}

func TestHaltCoverage_aRequeueScanIsMarkedAtTheObservationSite(t *testing.T) {
	t.Parallel()
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	testsupport.DamageTable(t, s.DB, s.Path, "delivery_intent")
	if _, err := d.Tick(ctx); err != nil {
		t.Fatalf("a requeue scan that met the damage returned %v instead of halting the store", err)
	}
	state := store.HaltStateAt(s.Path)
	if !state.Present || state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteObservation {
		t.Fatalf("the requeue's scan was not marked at the observation site: %+v", state)
	}
}

// Item 5: the ledger's writes in the sweep's recording are writes.
func TestHaltCoverage_aSweepRecordingWriteIsMarkedAtTheWriteSite(t *testing.T) {
	t.Parallel()
	d, s, ctx := haltCoverageDaemon(t, &observationHost{status: "completed"})
	attachManagedSettlement(t, s)
	d.Faults = &faults.Ledger{Store: s, Clock: d.Clock}
	d.Sweeper = &faults.Sweeper{Store: s, Selection: store.StateSelection{Path: filepath.Dir(s.Path)}, ManagedObserver: stubObserver{reading: omittedReport()}, Now: d.Clock.ISO, HostRecordPath: filepath.Join(filepath.Dir(s.Path), "absent-host-record")}
	testsupport.DamageTable(t, s.DB, s.Path, "fault_ledger")
	if _, err := d.Tick(ctx); err != nil {
		t.Fatalf("a recording write that met the damage returned %v instead of halting the store", err)
	}
	state := store.HaltStateAt(s.Path)
	if !state.Present || state.Marker.Code != 11 || state.Marker.Site != store.HaltSiteWrite {
		t.Fatalf("the sweep's ledger write was not marked at the write site: %+v", state)
	}
}

// stubObserver is a faults.ManagedReadingObserver with a fixed answer; before runs first.
type stubObserver struct {
	reading map[string]any
	err     error
	before  func()
}

func (o stubObserver) Observe(context.Context, faults.ManagedReadingRequest) (any, error) {
	if o.before != nil {
		o.before()
	}
	return o.reading, o.err
}

// omittedReport is a reading the sweep records a fault for.
func omittedReport() map[string]any {
	return map[string]any{"schema": delivery.OmittedSchema, "reportingState": "unreported", "reason": delivery.OmittedReason, "owed": true, "owedReason": delivery.OmittedReason, "relationshipId": "r", "observedAt": "2023-11-14T22:13:20.000000+00:00",
		"selectors": map[string]any{"session": "child", "turn": "business"}, "executionGeneration": 1, "parentTaskId": "parent", "relationshipStatus": "active", "turnAdmission": "admitted"}
}

// attachManagedSettlement gives the fault sweep a settled turn of an attached managed request to read.
func attachManagedSettlement(t *testing.T, s *store.Store) {
	t.Helper()
	exec(t, s, "INSERT INTO managed_start_requests(request_id,issue_key,request_fingerprint,fingerprint_version,workspace,marker_root,socket_identity,create_request_id,dispatch_request_id,state,revision,child_task_id,standby_turn_id,relationship_id,execution_generation,receipt_status,created_at,updated_at) VALUES('req-1','REL-1','fp','v1','/child','/markers','sock','create-1','dispatch-r','attached',3,'child','standby','r',1,'accepted','2023-11-14T22:13:20Z','2023-11-14T22:13:20Z')")
	exec(t, s, "INSERT INTO assignment_settlements VALUES('r','child','business','completed','2023-11-14T22:13:20Z')")
}
