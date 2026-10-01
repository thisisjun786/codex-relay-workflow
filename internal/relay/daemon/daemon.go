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
}

func New(s *store.Store, host Host, clock delivery.Clock, channel *supervisor.Channel) *Daemon {
	d := delivery.NewService(s, clock)
	ack := delivery.NewAck(d)
	rc := delivery.NewReconciler(d)
	return &Daemon{Store: s, Host: host, Clock: clock, Delivery: d, Ack: ack, Reconciler: rc, Intake: store.ReceiptIntake{Store: s, Now: clock.ISO}, Channel: channel, Policy: DefaultPolicy(), checks: &delivery.TurnChecks{Reconciler: rc, Budget: 4}}
}
func (d *Daemon) Tick(ctx context.Context) (Report, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	r := Report{Notes: []string{}}
	now := d.Clock.Now()
	bind := func() {
		bound, err := d.Ack.BindPendingAnchors(ctx)
		if err != nil {
			r.Notes = append(r.Notes, "anchor recovery failed: "+err.Error())
		} else {
			r.AnchorsBound += len(bound)
		}
	}
	bind()
	if err := d.observe(ctx, &r); err != nil {
		return r, err
	}
	d.sweep(ctx, &r)
	d.requeue(ctx, &r, now)
	var rr delivery.ReconcileReport
	if err := delivery.ReconcilePass(ctx, d.Reconciler, d.Host, d.Policy.MaxReconciles, now, &rr); err != nil {
		return r, err
	}
	r.Reconciled += rr.Reconciled
	r.Skipped += rr.Skipped
	r.Notes = append(r.Notes, rr.Notes...)
	bind()
	r.Notes = append(r.Notes, delivery.ConfirmKeptAcks(ctx, d.Ack, d.Reconciler, d.Host, now)...)
	results, err := d.Ack.VerifyPendingAcks(ctx, d.Host, 8, &now)
	if err != nil {
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
	var sent delivery.TickCounts
	if err := (&delivery.Scheduler{Delivery: d.Delivery, Ack: d.Ack, MaxSendsTick: d.Policy.MaxSends}).Deliver(ctx, d.Host, now, &sent); err != nil {
		return r, err
	}
	r.Delivered += sent.Delivered
	r.Deferred += sent.Deferred
	r.Skipped += sent.Skipped
	r.Notes = append(r.Notes, sent.Notes...)
	if d.Channel != nil {
		a, err := d.Channel.AutoSend(ctx, d.Host, now, d.Policy.MaxProjects, d.Policy.MaxSupervisorSends, d.afterProject, d.afterStaged, d.afterMessage)
		if err != nil {
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
func (d *Daemon) requeue(ctx context.Context, r *Report, now float64) {
	rows, err := d.Store.All(ctx, "SELECT i.* FROM delivery_intent i LEFT JOIN deliveries d ON d.event_id=i.event_id WHERE d.event_id IS NULL AND (i.next_retry_at IS NULL OR i.next_retry_at<=?) ORDER BY i.next_retry_at LIMIT ?", now, d.Policy.MaxSends)
	if err != nil {
		r.Notes = append(r.Notes, "requeue scan failed: "+err.Error())
		return
	}
	for _, row := range rows {
		event := row.Get("event_id").(string)
		_, err := d.Delivery.Enqueue(ctx, event, row.Get("kind").(string), row.Get("recipient_task_id").(string))
		if err != nil {
			r.Notes = append(r.Notes, "requeue refused for "+event+": "+err.Error())
			err = d.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
				return d.Delivery.RecordIntentIn(tx, event, row.Get("relationship_id").(string), row.Get("kind").(string), row.Get("recipient_task_id").(string), err.Error(), now)
			})
			if err != nil {
				r.Notes = append(r.Notes, "requeue intent failed for "+event+": "+err.Error())
			}
			continue
		}
		r.Requeued++
	}
}
func (d *Daemon) sweep(ctx context.Context, r *Report) {
	if d.Faults == nil || d.Sweeper == nil {
		return
	}
	batch, err := d.Sweeper.Sweep(ctx, "crw")
	if err != nil {
		r.Notes = append(r.Notes, "fault sweep failed: "+err.Error())
		return
	}
	answer, err := d.Sweeper.RecordAll(ctx, d.Faults, batch)
	if err != nil {
		r.Notes = append(r.Notes, "fault sweep failed: "+err.Error())
		return
	}
	r.FaultsRecorded += answer.Recorded
	for _, one := range answer.Gaps {
		gap := one.(map[string]any)
		r.Notes = append(r.Notes, fmt.Sprintf("fault %v: %v", gap["gap"], gap["reason"]))
	}
	attention, err := d.Faults.Attention(ctx)
	if err != nil {
		r.Notes = append(r.Notes, "fault sweep failed: "+err.Error())
		return
	}
	warning, _ := attention["warning"].(string)
	if warning != "" && warning != d.lastAttention {
		r.Notes = append(r.Notes, warning)
	}
	d.lastAttention = warning
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
