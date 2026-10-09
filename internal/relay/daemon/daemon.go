package daemon

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

type Host interface {
	delivery.Adapter
	supervisor.SendAdapter
	Close() error
}
type Report struct {
	Observed, Reconciled, Delivered, Deferred, Skipped, AcksVerified, AnchorsBound, Requeued, FaultsRecorded, SupervisorStaged, SupervisorSent, NotificationsDelivered, TurnsLost, TurnsUndecided int
	Notes                                                                                                                                                                                         []string
}

func (r Report) Quiet() bool {
	return r.Observed+r.Reconciled+r.Delivered+r.Deferred+r.AcksVerified+r.AnchorsBound+r.Requeued+r.FaultsRecorded+r.SupervisorStaged+r.SupervisorSent+r.NotificationsDelivered+r.TurnsLost+r.TurnsUndecided == 0
}
func (r Report) Object() contract.OrderedObject {
	notes := []any{}
	for _, n := range r.Notes {
		notes = append(notes, n)
	}
	return contract.OrderedObject{{Key: "observed", Value: r.Observed}, {Key: "reconciled", Value: r.Reconciled}, {Key: "delivered", Value: r.Delivered}, {Key: "deferred", Value: r.Deferred}, {Key: "skipped", Value: r.Skipped}, {Key: "acksVerified", Value: r.AcksVerified}, {Key: "anchorsBound", Value: r.AnchorsBound}, {Key: "requeued", Value: r.Requeued}, {Key: "faultsRecorded", Value: r.FaultsRecorded}, {Key: "supervisorStaged", Value: r.SupervisorStaged}, {Key: "supervisorSent", Value: r.SupervisorSent}, {Key: "notificationsDelivered", Value: r.NotificationsDelivered}, {Key: "turnsLost", Value: r.TurnsLost}, {Key: "turnsUndecided", Value: r.TurnsUndecided}, {Key: "quiet", Value: r.Quiet()}, {Key: "notes", Value: notes}}
}

// Policy bounds what one tick does. The observation pass (observe.go) reads at most MaxTurnReads turns, no
// relationship more than its share of them (never less than MinRelationshipShare), and stops reading after
// MaxObserveSeconds, apart from the first read of each class of turn; zero turns that time limit off.
type Policy struct {
	PollInterval, MaxObserveSeconds                                                                             float64
	MaxSends, MaxReconciles, MaxTurnReads, MaxTurnChecks, MinRelationshipShare, MaxProjects, MaxSupervisorSends int
}

// DefaultPolicy: a read of one turn cost a mean of 2.3 ms at the median thread and 17 ms at the worst of 40 recent
// threads, measured read-only against the App Server (CRW-258), so 32 reads take under a second of a 20 s tick.
func DefaultPolicy() Policy {
	return Policy{PollInterval: 20, MaxObserveSeconds: 10, MaxSends: 4, MaxReconciles: 8, MaxTurnReads: 32, MaxTurnChecks: 4, MinRelationshipShare: 2, MaxProjects: 4, MaxSupervisorSends: 2}
}

type Daemon struct {
	Store      *store.Store
	Delivery   *delivery.Service
	Ack        *delivery.Ack
	Reconciler *delivery.Reconciler
	Intake     store.ReceiptIntake
	Host       Host
	Channel    *supervisor.Channel
	Clock      delivery.Clock
	Policy     Policy
	Faults     *faults.Ledger
	Sweeper    *faults.Sweeper
	Notices    *faults.NoticeDeliverer
	// Tick owns the rotating in-memory state; concurrent callers serialize here.
	mu                                                     sync.Mutex
	checks                                                 *delivery.TurnChecks
	afterProject, afterStaged, afterMessage, lastAttention string
	// mono is the clock the observation time limit runs on; nil is time.Now, whose monotonic reading no wall
	// clock step can change. Tests move it by hand.
	mono func() time.Time
	// idle is the idle-edge pass of CRW-904: the status reports the host pushed for recipients whose
	// head delivery waits out a busy backoff, and the subscription held on each of them.
	idle *idleWake
	// beforeSettle is called, when set, once the end of a turn is judged and before the settlement commits.
	// Tests move the store in that gap by hand; production leaves it nil.
	beforeSettle func(store.TurnReference)
	// haltedStore is this process's own halt: once a pass has seen the class, no later pass attempts a
	// write whatever became of the marker, because a state directory that cannot be written must not
	// turn a halt back into writes. haltReason is what the pass says about it.
	haltedStore bool
	haltReason  string
}

func New(s *store.Store, host Host, clock delivery.Clock, channel *supervisor.Channel) *Daemon {
	d := delivery.NewService(s, clock)
	ack := delivery.NewAck(d)
	rc := delivery.NewReconciler(d)
	daemon := &Daemon{Store: s, Host: host, Clock: clock, Delivery: d, Ack: ack, Reconciler: rc, Intake: store.ReceiptIntake{Store: s, Now: clock.ISO}, Channel: channel, Policy: DefaultPolicy(), checks: &delivery.TurnChecks{Reconciler: rc, Budget: 4}}
	daemon.idle = newIdleWake(daemon)
	return daemon
}
func (d *Daemon) Tick(ctx context.Context) (Report, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := Report{Notes: []string{}}
	// A store whose writes are halted gets a pass that writes nothing at all (CRW-848): no read of
	// the host, no settlement, no delivery, no journal row. The deliveries this daemon was carrying
	// stay where they are, for the next daemon and for the operator's restore.
	if d.haltedStore {
		r.Notes = append(r.Notes, d.haltReason)
		return r, nil
	}
	if state := store.HaltStateAt(d.Store.Path); state.Present {
		r.Notes = append(r.Notes, haltNote(state))
		return r, nil
	}
	now := d.Clock.Now()
	bind := func() bool {
		bound, err := d.Ack.BindPendingAnchors(ctx)
		if err != nil {
			if d.halted(ctx, &r, store.HaltSiteWrite, err) {
				return true
			}
			r.Notes = append(r.Notes, "anchor recovery failed: "+err.Error())
		} else {
			r.AnchorsBound += len(bound)
		}
		return false
	}
	if bind() {
		return r, nil
	}
	if err := d.observe(ctx, &r); err != nil {
		if d.halted(ctx, &r, store.HaltSiteObservation, err) {
			return r, nil
		}
		return r, err
	}
	if d.haltedStore {
		// A settlement of the observation pass halted the store and the pass ended on it (CRW-945): nothing
		// after it - the sweep, the requeue, the reconciliation, the delivery - is attempted on that store.
		return r, nil
	}
	if err := d.sweep(ctx, &r); err != nil && d.halted(ctx, &r, store.HaltSiteObservation, err) {
		return r, nil
	}
	if d.haltedStore {
		return r, nil
	}
	if err := d.requeue(ctx, &r, now); err != nil && d.halted(ctx, &r, store.HaltSiteObservation, err) {
		return r, nil
	}
	var rr delivery.ReconcileReport
	if err := delivery.ReconcilePass(ctx, d.Reconciler, d.Host, d.Policy.MaxReconciles, now, &rr); err != nil {
		if d.halted(ctx, &r, store.HaltSiteWrite, err) {
			return r, nil
		}
		return r, err
	}
	r.Reconciled += rr.Reconciled
	r.Skipped += rr.Skipped
	r.Notes = append(r.Notes, rr.Notes...)
	if bind() {
		return r, nil
	}
	r.Notes = append(r.Notes, delivery.ConfirmKeptAcks(ctx, d.Ack, d.Reconciler, d.Host, now)...)
	results, err := d.Ack.VerifyPendingAcks(ctx, d.Host, 8, &now)
	if err != nil {
		if d.halted(ctx, &r, store.HaltSiteWrite, err) {
			return r, nil
		}
		r.Notes = append(r.Notes, "pending acknowledgement pass failed: "+err.Error())
	} else {
		for _, result := range results {
			o := result.(contract.OrderedObject)
			if value(o, "outcome") == "verified" {
				r.AcksVerified++
			}
			if value(o, "outcome") == "withheld" {
				r.Notes = append(r.Notes, fmt.Sprintf("acknowledgement %v not promoted: %v", value(o, "eventId"), value(o, "reason")))
			}
		}
	}
	d.checks.Budget = d.Policy.MaxTurnChecks
	var tc delivery.TurnCheckReport
	d.checks.Pass(ctx, d.Host, now, &tc)
	r.TurnsLost += tc.TurnsLost
	r.TurnsUndecided += tc.TurnsUndecided
	r.Notes = append(r.Notes, tc.Notes...)
	// The idle edge (CRW-904). The status reports the host pushed since the last tick release the
	// heads they name before this tick's delivery pass, and the subscription below is opened for
	// every recipient the pass leaves waiting out a busy backoff. Both are no-ops on a host that
	// does not offer them: every scripted double, and every transport that is not the App Server.
	d.idle.begin()
	if err := d.idle.idle(ctx, &r, d.Host, now); err != nil {
		// The wake is this pass's own write (CRW-1007, decision 2): a corrupting failure halts the store at the
		// write site and ends the tick before the delivery pass and the supervisor channel write anything (I-564).
		// A failure that is not corruption stays the tick's error.
		r.Notes = append(r.Notes, d.idle.take()...)
		if d.halted(ctx, &r, store.HaltSiteWrite, err) {
			return r, nil
		}
		return r, err
	}
	var sent delivery.TickCounts
	if err := (&delivery.Scheduler{Delivery: d.Delivery, Ack: d.Ack, MaxSendsTick: d.Policy.MaxSends}).Deliver(ctx, d.Host, now, &sent); err != nil {
		if d.halted(ctx, &r, store.HaltSiteWrite, err) {
			return r, nil
		}
		return r, err
	}
	if err := d.idle.hold(ctx, &r, d.Host, now); err != nil {
		// The waiting heads are read out of the store after the delivery pass (CRW-1007, decision 2): a corrupting
		// read halts the store at the observation site, and the supervisor channel does not write after it.
		r.Notes = append(r.Notes, d.idle.take()...)
		if d.halted(ctx, &r, store.HaltSiteObservation, err) {
			// The delivery pass ran before this read, so its counts and notes are this tick's: the halt keeps them.
			r.Delivered += sent.Delivered
			r.Deferred += sent.Deferred
			r.Skipped += sent.Skipped
			r.Notes = append(r.Notes, sent.Notes...)
			return r, nil
		}
		return r, err
	}
	r.Notes = append(r.Notes, d.idle.take()...)
	r.Delivered += sent.Delivered
	r.Deferred += sent.Deferred
	r.Skipped += sent.Skipped
	r.Notes = append(r.Notes, sent.Notes...)
	if d.Channel != nil {
		a, err := d.Channel.AutoSend(ctx, d.Host, now, d.Policy.MaxProjects, d.Policy.MaxSupervisorSends, d.afterProject, d.afterStaged, d.afterMessage)
		if err != nil {
			if d.halted(ctx, &r, store.HaltSiteWrite, err) {
				return r, nil
			}
			return r, err
		}
		d.afterProject, d.afterStaged, d.afterMessage = a.AfterProject, a.AfterStagedAt, a.AfterMessageID
		r.SupervisorStaged += a.SupervisorStaged
		r.SupervisorSent += a.SupervisorSent
		r.Deferred += a.Deferred
		r.Skipped += a.Skipped
		r.Notes = append(r.Notes, a.Notes...)
		if d.Notices != nil {
			answer, err := d.Notices.Tick(ctx, now, max(0, d.Policy.MaxSupervisorSends-a.Attempts))
			if err != nil {
				r.Notes = append(r.Notes, "fault notifications not delivered: "+err.Error())
			} else {
				r.NotificationsDelivered += answer.Delivered
				r.Deferred += answer.Returned
				r.Observed += answer.Measured
				for _, one := range answer.Waiting {
					r.Notes = append(r.Notes, "fault notification "+one[0]+" waits: "+one[1])
				}
			}
		}
	}
	return r, nil
}
func value(o contract.OrderedObject, key string) any {
	for _, f := range o {
		if f.Key == key {
			return f.Value
		}
	}
	return nil
}

// halted marks the store and reports whether err was a failure of the class that stops writes
// (halt.go, CRW-848). The marker is published for the write site and the pass that saw it ends: the
// caller returns without an error, so the run keeps ticking and every later pass is write-free
// because Tick checks the marker first. A failure that is not the class is left to the caller
// exactly as it was. The marker's own failure is a note, never a second failure that would hide the
// first.
func (d *Daemon) halted(ctx context.Context, r *Report, site string, err error) bool {
	if d.haltedStore {
		// Already halted: the site of the first detection stands, and no second marker is
		// published for the same damage.
		return true
	}
	cause, ok := store.CorruptingFailure(err)
	if !ok {
		return false
	}
	// The step that met the failure knows whether it was reading or writing; the caller's site is the one
	// of the sub-pass it called, which is right only when the sub-pass does both (CRW-945).
	cause.Site = store.SiteOf(err, site)
	reason := ""
	if recordErr := store.RecordHalt(ctx, d.Store.Path, cause); recordErr != nil {
		reason = "store writes are halted, and the halt marker could not be written: " + recordErr.Error()
	} else {
		reason = haltNote(store.HaltStateAt(d.Store.Path))
	}
	// The in-process halt stands whatever became of the marker: a marker that could not be written
	// must not let the next pass treat the damaged store as healthy.
	d.haltedStore, d.haltReason = true, reason
	r.Notes = append(r.Notes, reason)
	return true
}

// adoptMarker takes over a halt marker that appeared during the pass, from the observation path or another
// process, as this process's own halt, and reports whether there was one.
func (d *Daemon) adoptMarker(r *Report) bool {
	if d.haltedStore {
		return true
	}
	state := store.HaltStateAt(d.Store.Path)
	if !state.Present {
		return false
	}
	d.haltedStore, d.haltReason = true, haltNote(state)
	r.Notes = append(r.Notes, d.haltReason)
	return true
}

// haltNote is what a pass says about a halted store.
func haltNote(state store.HaltState) string {
	if state.Detail != "" {
		return "store writes are halted: " + state.Detail + " (" + state.Path + ")"
	}
	return fmt.Sprintf("store writes are halted: %s records %s (code %d) seen at %s on %s; no write is attempted until it is cleared by hand",
		state.Path, state.Marker.Message, state.Marker.Code, state.Marker.Site, state.Marker.DetectedAt)
}

// requeue re-enqueues the intents whose delivery never landed. A scan that fails is a note for
// every other failure and the error itself when the store is damaged, so Tick halts the pass
// (CRW-848).
func (d *Daemon) requeue(ctx context.Context, r *Report, now float64) error {
	rows, err := d.Store.All(ctx, "SELECT i.* FROM delivery_intent i LEFT JOIN deliveries d ON d.event_id=i.event_id WHERE d.event_id IS NULL AND (i.next_retry_at IS NULL OR i.next_retry_at<=?) ORDER BY i.next_retry_at LIMIT ?", now, d.Policy.MaxSends)
	if err != nil {
		if _, corrupting := store.CorruptingFailure(err); corrupting {
			return err
		}
		r.Notes = append(r.Notes, "requeue scan failed: "+err.Error())
		return nil
	}
	for _, row := range rows {
		event := row.Get("event_id").(string)
		_, err := d.Delivery.Enqueue(ctx, event, row.Get("kind").(string), row.Get("recipient_task_id").(string))
		if err != nil {
			if _, corrupting := store.CorruptingFailure(err); corrupting {
				return store.AtSite(store.HaltSiteWrite, err)
			}
			r.Notes = append(r.Notes, "requeue refused for "+event+": "+err.Error())
			err = d.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
				return d.Delivery.RecordIntentIn(tx, event, row.Get("relationship_id").(string), row.Get("kind").(string), row.Get("recipient_task_id").(string), err.Error(), now)
			})
			if err != nil {
				if _, corrupting := store.CorruptingFailure(err); corrupting {
					return store.AtSite(store.HaltSiteWrite, err)
				}
				r.Notes = append(r.Notes, "requeue intent failed for "+event+": "+err.Error())
			}
			continue
		}
		r.Requeued++
	}
	return nil
}

// sweep records the fault ledger's batch. Its failures are notes, apart from a damaged store,
// whose error is returned so Tick halts the pass (CRW-848).
func (d *Daemon) sweep(ctx context.Context, r *Report) error {
	if d.Faults == nil || d.Sweeper == nil {
		return nil
	}
	batch, err := d.Sweeper.Sweep(ctx, "crw")
	if err != nil {
		return sweepFailure(r, err)
	}
	// The sweep's readings may have published the halt marker (the omission observer does, CRW-848): the
	// marker is the halt, so the recording that follows is not attempted on a store the sweep has just seen
	// damaged (CRW-945).
	if d.adoptMarker(r) {
		return nil
	}
	answer, err := d.Sweeper.RecordAll(ctx, d.Faults, batch)
	if err != nil {
		return sweepFailure(r, err)
	}
	r.FaultsRecorded += answer.Recorded
	for _, one := range answer.Gaps {
		gap := one.(map[string]any)
		r.Notes = append(r.Notes, fmt.Sprintf("fault %v: %v", gap["gap"], gap["reason"]))
	}
	attention, err := d.Faults.Attention(ctx)
	if err != nil {
		return sweepFailure(r, err)
	}
	warning, _ := attention["warning"].(string)
	if warning != "" && warning != d.lastAttention {
		r.Notes = append(r.Notes, warning)
	}
	d.lastAttention = warning
	return nil
}

// sweepFailure is a fault sweep failure: the error itself when the store is damaged, so Tick halts
// the pass, and otherwise the note it has always been.
func sweepFailure(r *Report, err error) error {
	if _, corrupting := store.CorruptingFailure(err); corrupting {
		return err
	}
	r.Notes = append(r.Notes, "fault sweep failed: "+err.Error())
	return nil
}

// Run is bounded by count or wall deadline; stop is additional, never a bound.
func Run(ctx context.Context, tick func(context.Context) (Report, error), clock delivery.Clock, interval float64, maxTicks *int, deadline *float64, stop func() bool, sleep func(float64) error) ([]Report, error) {
	if maxTicks == nil && deadline == nil {
		return nil, fmt.Errorf("run() needs max_ticks or deadline; a stop callable is not a bound because it may never return True")
	}
	reports := []Report{}
	for {
		if maxTicks != nil && len(reports) >= *maxTicks {
			break
		}
		if deadline != nil && clock.Now() >= *deadline {
			break
		}
		if stop != nil && stop() {
			break
		}
		if err := ctx.Err(); err != nil {
			return reports, err
		}
		r, err := tick(ctx)
		if err != nil {
			return reports, err
		}
		reports = append(reports, r)
		if sleep != nil && (maxTicks == nil || len(reports) < *maxTicks) {
			if err = sleep(interval); err != nil {
				return reports, err
			}
		}
	}
	return reports, nil
}
func SchedulerWait(ctx context.Context, clock delivery.Clock, deadline *float64, seconds float64) error {
	if deadline != nil {
		seconds = min(seconds, *deadline-clock.Now())
	}
	if seconds <= 0 {
		return nil
	}
	timer := time.NewTimer(time.Duration(seconds * float64(time.Second)))
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
