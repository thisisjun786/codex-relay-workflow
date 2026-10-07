package daemon

import (
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (d *Daemon) worth(ctx context.Context, thread, turn, rid string) (bool, error) {
	var found int
	err := d.Store.Q(ctx).QueryRowContext(ctx, "SELECT 1 FROM events WHERE stage='staged' AND turn_thread_id=? AND turn_id=? AND relationship_id=? LIMIT 1", thread, turn, rid).Scan(&found)
	if err == nil {
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	err = d.Store.Q(ctx).QueryRowContext(ctx, "SELECT 1 FROM assignment_settlements WHERE relationship_id=? AND thread_id=? AND turn_id=?", rid, thread, turn).Scan(&found)
	return errors.Is(err, sql.ErrNoRows), func() error {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}()
}

// monotonic is the clock the time bound of a pass runs on: a wall clock that steps cannot lengthen it.
func (d *Daemon) monotonic() time.Time {
	if d.mono != nil {
		return d.mono()
	}
	return time.Now()
}

// pass is one tick's observation: the reads it has made against its budget, and when it began.
type pass struct {
	d       *Daemon
	report  *Report
	start   time.Time
	floor   int // reads made whatever the clock says
	reads   int
	stopped bool
	// until is the instant the time bound of the pass ends the host read about to be made, as the last spent
	// reading put it, and the zero time when that read is not bounded by it: one of the first reads, or no
	// bound at all. An instant already past is a bound that has run out, never an unbounded read.
	until     time.Time
	relations map[string]delivery.Relationship
	used      [2]map[string]int // reads this tick, by class and relationship
	// deferred are the turns this pass read that ended failed or interrupted, in the order they were read. Settling
	// such a turn asks whether a later turn of its generation holds a final child receipt (laterReceipt), and what
	// turns a staged claim into one is the settlement of a completed turn, so these ends wait until the pass has
	// read what it will read: a later turn's claim that the pass confirms is then counted, whichever of the two
	// turns the schedule read first. Among themselves their order does not matter, since settling a failed or
	// interrupted turn leaves no child receipt for the lookup to count. (Suppressing its staged claim can still
	// change which revision of a forked lineage is current, and so what the lookup answers for another turn;
	// that was so when each end was settled as it was read, and is not changed here.)
	deferred []endedTurn
}

// endedTurn is a turn that ended failed or interrupted, with the relationship it was read for.
type endedTurn struct {
	r    delivery.Relationship
	turn store.TurnReference
}

// settleDeferred settles the ends that waited for the reads of the pass.
func (o *pass) settleDeferred(ctx context.Context) {
	for _, e := range o.deferred {
		o.d.settle(ctx, e.r, e.turn, o.report)
	}
	o.deferred = nil
}

// spent reports whether the pass has used the time Policy.MaxObserveSeconds allows it, and says so once. The
// first reads, one for each class that waits, are made whatever the clock says, so a slow class cannot keep the
// other out of the tick.
//
// A read that goes ahead is bounded by the time that is left: spent records the instant the pass runs out (until),
// and the host call of that read ends then, as the clock check would end the pass before its next read. The instant
// is the real clock's now plus the time left on the monotonic clock, from the one reading spent made, so a test that
// moves the monotonic clock by hand gets a bound of the time it says is left. A read that is cut off is the last of
// the pass; a pass with no turn left to read ends without asking the clock again, and does not say why it stopped.
func (o *pass) spent() bool {
	o.until = time.Time{}
	if limit := o.d.Policy.MaxObserveSeconds; !o.stopped && limit > 0 && o.reads >= o.floor {
		if took := o.d.monotonic().Sub(o.start).Seconds(); took >= limit {
			o.stopped = true
			o.report.Notes = append(o.report.Notes, fmt.Sprintf("observation stopped after %.1f s, the limit for one pass; the other turns wait for the next tick", took))
		} else {
			o.until = time.Now().Add(time.Duration((limit - took) * float64(time.Second)))
		}
	}
	return o.stopped
}

// observe reads the host for the turns that still need it, as the census finds them from store rows alone: a
// relationship with nothing to observe is neither loaded nor read. Reads are scheduled per turn and shared out per
// relationship. A turn a staged claim waits on (class A) is read before the others (class B), but one read of B
// goes first, so A can never take the budget whole, and inside a class the relationships that have waited longest
// deal one turn each, round after round. No relationship gets more than its share of a class in a tick, which is
// what the budget allows each relationship that needs it and never less than Policy.MinRelationshipShare. The pass
// is bounded by Policy.MaxTurnReads reads and Policy.MaxObserveSeconds of time.
//
// A turn that ended completed is settled as it is read. One that ended failed or interrupted waits until the pass
// has read what it will read (pass.deferred), so that a later turn's staged claim confirmed in the same pass is
// counted whichever of the two turns was read first.
func (d *Daemon) observe(ctx context.Context, report *Report) error {
	budget := d.Policy.MaxTurnReads
	work, err := d.census(ctx)
	if err != nil || len(work) == 0 || budget <= 0 {
		return err
	}
	staged, others := deal(work, 1), deal(work, 0)
	order, floor := slices.Concat(staged, others), 1
	switch {
	case len(staged) == 0 || len(others) == 0:
	case budget < 2:
		// One read serves the longest-waiting turn of either class.
		slices.SortStableFunc(order, func(a, b read) int {
			return cmp.Or(cmp.Compare(a.t.waiting(), b.t.waiting()), cmp.Compare(a.p.id, b.p.id), cmp.Compare(a.t.how, b.t.how), cmp.Compare(a.t.pos, b.t.pos))
		})
	default:
		order, floor = slices.Concat(others[:1], staged, others[1:]), 2
	}
	share := max(1, d.Policy.MinRelationshipShare, budget/len(work))
	o := &pass{d: d, report: report, start: d.monotonic(), floor: floor, relations: map[string]delivery.Relationship{}, used: [2]map[string]int{{}, {}}}
	// Whatever ends the loop (the budget, the time, a failure), the failed and interrupted ends it has read are
	// settled before it returns, as each was when it was read. A cancelled context refuses every store call, and
	// leaves them to the next tick.
	defer func() {
		if ctx.Err() == nil {
			o.settleDeferred(ctx)
		}
	}()
	for _, next := range order {
		if o.reads >= budget || o.spent() {
			break
		}
		if o.used[next.class][next.p.id] >= share {
			continue
		}
		r, ok := o.relations[next.p.id]
		if !ok {
			if r, err = delivery.LoadRelationship(ctx, d.Store, next.p.id); err != nil {
				return err
			}
			o.relations[next.p.id] = r
		}
		o.reads++
		o.used[next.class][next.p.id]++
		var turn *store.TurnReference
		if turn, err = d.read(ctx, o.until, r, next.t.id, report); err != nil {
			return err
		}
		switch {
		case turn == nil:
		case turn.Status == "completed":
			d.settle(ctx, r, *turn, report)
		default:
			o.deferred = append(o.deferred, endedTurn{r, *turn})
		}
	}
	return nil
}

// readHost asks the host about one turn under the instant the pass's time bound ends the read, when it has one. Only
// the host call runs under it: what the pass records afterwards belongs to the tick.
func (d *Daemon) readHost(ctx context.Context, until time.Time, thread, turn string) (*delivery.TurnInfo, error) {
	if !until.IsZero() {
		var cancel context.CancelFunc
		ctx, cancel = context.WithDeadline(ctx, until)
		defer cancel()
	}
	return d.Host.ReadTurn(ctx, thread, turn)
}

// read asks the host about one turn of r, before until when that is set, and records the attempt. It returns the turn
// when it ended and there is something to settle for it, and nil otherwise.
func (d *Daemon) read(ctx context.Context, until time.Time, r delivery.Relationship, turnID string, report *Report) (_ *store.TurnReference, err error) {
	turn, e := d.readHost(ctx, until, r.Child.TaskID, turnID)
	var status, pollErr any
	if e != nil {
		report.Notes = append(report.Notes, "turn read failed for "+turnID+": "+e.Error())
		pollErr = "Exception: " + e.Error()
	} else if turn == nil {
		status = "absent"
		pollErr = "str: the host reports this turn absent"
	} else {
		status = turn.Status
	}
	now := d.Clock.ISO()
	var success any
	if pollErr == nil {
		success = now
	}
	if err = d.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		_, err := d.Store.Q(tx).ExecContext(tx, "INSERT INTO poll_observations (relationship_id,execution_generation,turn_id,last_status,last_polled_at,last_attempt_at,last_error) VALUES (?,?,?,?,?,?,?) ON CONFLICT(relationship_id,execution_generation,turn_id) DO UPDATE SET last_status=excluded.last_status,last_polled_at=COALESCE(excluded.last_polled_at,poll_observations.last_polled_at),last_attempt_at=excluded.last_attempt_at,last_error=excluded.last_error", r.ID, r.Generation, turnID, status, success, now, pollErr)
		return err
	}); err != nil {
		// The observation pass writes this row, so a failure here is the write site (CRW-848): the
		// marker records where the relay's own statement met the damage, not which sub-pass it was
		// in. The first detection's site stands, and the pass ends on the returned error.
		d.halted(ctx, report, store.HaltSiteWrite, err)
		return nil, err
	}
	// Python: turn.status not in ("completed", "failed", "interrupted"); a
	// non-string host status equals none of them.
	final, _ := statusText(turn)
	if e != nil || turn == nil || !slices.Contains([]string{"completed", "failed", "interrupted"}, final) {
		return nil, nil
	}
	worth, e := d.worth(ctx, r.Child.TaskID, turnID, r.ID)
	if e != nil || !worth {
		return nil, e
	}
	return &store.TurnReference{ThreadID: r.Child.TaskID, TurnID: turnID, Status: final}, nil
}

// errNotSilenced is what the settlement transaction answers when the receipt that silenced the end of a turn
// no longer does by the time the settlement commits.
var errNotSilenced = errors.New("the receipt that silenced the end of the turn no longer holds")

var errManagedStandby = errors.New("the turn is the inert original managed standby")

// isManagedStandby repeats the census association under the settlement writer
// lock. Selection can predate attachment, or reach the turn through an admission
// or staged claim instead of the anchor query. None licenses a standby settlement.
func (d *Daemon) isManagedStandby(ctx context.Context, rid string, turn store.TurnReference) (bool, error) {
	var found bool
	err := d.Store.Q(ctx).QueryRowContext(ctx, `SELECT EXISTS (
SELECT 1 FROM relationships r JOIN generations g ON g.relationship_id=r.relationship_id
WHERE r.relationship_id=? AND r.child_task_id=? AND g.dispatch_turn_id=?
  AND EXISTS (`+managedStandby+`))`, rid, turn.ThreadID, turn.TurnID).Scan(&found)
	return found, err
}

func (d *Daemon) settle(ctx context.Context, r delivery.Relationship, turn store.TurnReference, report *Report) {
	synthesized, laterTurn, laterEvent, ownEvent := "", "", "", ""
	observationDeferred := false
	ended := turn.Status == "failed" || turn.Status == "interrupted"
	if ended {
		var err error
		if ownEvent, err = d.ownReceipt(ctx, r.ID, turn); err != nil {
			report.Notes = append(report.Notes, "own receipt lookup failed for "+turn.TurnID+": "+err.Error())
			return
		}
		if ownEvent == "" {
			if laterTurn, laterEvent, err = d.laterReceipt(ctx, r.ID, turn); err != nil {
				report.Notes = append(report.Notes, "later receipt lookup failed for "+turn.TurnID+": "+err.Error())
				return
			}
		}
	}
	// When this turn already holds its own final child receipt whose delivery the parent is owed, or a later
	// turn of this generation holds one, the end of this turn is not news: the turn is settled below with no
	// event, so it is not read again. That rests on the lookups above, which were made before the settlement
	// transaction opens, so the transaction makes them again.
	commit := func(queue bool, refusal error) error {
		return d.Store.Compose(ctx, func(tx context.Context, conn *sql.Conn) error {
			standby, err := d.isManagedStandby(tx, r.ID, turn)
			if err != nil {
				return err
			}
			if standby {
				return errManagedStandby
			}
			// The transaction holds the writer lock until it commits, so what it finds is what the
			// settlement is written on. The lookups are made again rather than trusted, so a receipt
			// that arrived after the first lookups silences the end here as well, and one that a
			// generation opened, a relationship paused or a superseded delivery has since taken away
			// leaves nothing to silence it: then the turn is not settled and is judged again on the
			// next pass.
			if ended {
				eventNow, err := d.ownReceipt(tx, r.ID, turn)
				if err != nil {
					return fmt.Errorf("own receipt lookup failed: %w", err)
				}
				if ownEvent != "" && eventNow == "" {
					return errNotSilenced
				}
				ownEvent = eventNow
			}
			if ownEvent == "" && (laterEvent != "" || ended) {
				turnNow, eventNow, err := d.laterReceipt(tx, r.ID, turn)
				if err != nil {
					return fmt.Errorf("later receipt lookup failed: %w", err)
				}
				if laterEvent != "" && eventNow == "" {
					return errNotSilenced
				}
				laterTurn, laterEvent = turnNow, eventNow
			}
			// New binds Intake to this same Store, so its nested transaction joins
			// Compose. Synthesis, settlement and enqueue now commit or roll back together.
			synthesized = ""
			if laterEvent == "" && ownEvent == "" && ended {
				receipt, err := d.Intake.DaemonObservation(tx, r.ID, turn)
				if err != nil {
					if store.RefusalReason(err) == store.ReasonRelationshipNotActive {
						observationDeferred = true
						return err
					}
					if store.RefusalReason(err) == "" {
						return err
					}
					report.Notes = append(report.Notes, "daemon observation refused: "+err.Error())
				} else {
					synthesized = receipt.EventID
				}
			}
			rows, err := d.Store.All(tx, "SELECT event_id FROM events WHERE stage='staged' AND turn_thread_id=? AND turn_id=? AND relationship_id=? ORDER BY first_seen_at", turn.ThreadID, turn.TurnID, r.ID)
			if err != nil {
				return err
			}
			finalized, suppressed := []any{}, []any{}
			now := d.Clock.ISO()
			for _, row := range rows {
				event := row.Get("event_id").(string)
				if turn.Status == "completed" {
					_, err = conn.ExecContext(tx, "UPDATE events SET stage='final',finalized_at=?,finalizing_status=? WHERE event_id=?", now, turn.Status, event)
					finalized = append(finalized, event)
				} else {
					_, err = conn.ExecContext(tx, "UPDATE events SET stage='suppressed',finalized_at=?,finalizing_status=?,suppressed_reason=? WHERE event_id=?", now, turn.Status, "the turn ended "+turn.Status+", so the staged claim is not promoted", event)
					suppressed = append(suppressed, event)
				}
				if err != nil {
					return err
				}
			}
			if len(rows) > 0 {
				detail := pyjson.Dumps(contract.OrderedObject{{Key: "finalized", Value: finalized}, {Key: "suppressed", Value: suppressed}, {Key: "status", Value: turn.Status}}, pyjson.Options{})
				if _, err = conn.ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES (?,'staged_resolved',?,?)", now, turn.TurnID, detail); err != nil {
					return err
				}
			}
			classification, _ := store.ClassifyObservation(turn.Status, nil)
			var event any
			if synthesized != "" {
				event = synthesized
			}
			if _, err = conn.ExecContext(tx, "INSERT OR IGNORE INTO observations(thread_id,turn_id,terminal_status,relationship_id,classification,event_id,observed_at) VALUES(?,?,?,?,?,?,?)", turn.ThreadID, turn.TurnID, turn.Status, r.ID, string(classification), event, now); err != nil {
				return err
			}
			if _, err = conn.ExecContext(tx, "INSERT OR IGNORE INTO assignment_settlements(relationship_id,thread_id,turn_id,terminal_status,settled_at) VALUES(?,?,?,?,?)", r.ID, turn.ThreadID, turn.TurnID, turn.Status, now); err != nil {
				return err
			}
			if ownEvent != "" || laterEvent != "" {
				detail := contract.OrderedObject{{Key: "status", Value: turn.Status}}
				if ownEvent != "" {
					detail = append(detail, contract.OrderedObject{{Key: "ownEvent", Value: ownEvent}}...)
				} else {
					detail = append(detail, contract.OrderedObject{{Key: "laterTurn", Value: laterTurn}, {Key: "laterEvent", Value: laterEvent}}...)
				}
				if _, err = conn.ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES (?,'observation_not_asserted',?,?)", now, turn.TurnID, pyjson.Dumps(detail, pyjson.Options{})); err != nil {
					return err
				}
			}
			if synthesized != "" {
				finalized = append(finalized, synthesized)
			}
			for _, v := range finalized {
				event := v.(string)
				if err = d.Delivery.AnnotatePredecessorsIn(tx, event); err != nil {
					return err
				}
				if queue {
					_, err = d.Delivery.Enqueue(tx, event, delivery.Completion, r.Parent.TaskID)
				} else {
					err = d.Delivery.RecordIntentIn(tx, event, r.ID, delivery.Completion, r.Parent.TaskID, refusal.Error(), d.Clock.Now())
				}
				if err != nil {
					return err
				}
			}
			return nil
		})
	}
	if d.beforeSettle != nil {
		d.beforeSettle(turn)
	}
	err := commit(true, nil)
	if errors.Is(err, errManagedStandby) {
		return
	}
	if observationDeferred {
		report.Notes = append(report.Notes, "observation deferred, "+r.ID+" is not active: "+err.Error())
		return
	}
	if errors.Is(err, errNotSilenced) {
		report.Notes = append(report.Notes, "settlement of "+turn.TurnID+" ("+turn.Status+") withdrawn: the later receipt that silenced it no longer holds; the turn is judged again on the next pass")
		return
	}
	if err != nil {
		var refused *store.RefusedError
		if errors.As(err, &refused) {
			report.Notes = append(report.Notes, "enqueue refused for "+turn.TurnID+": "+err.Error())
			err = commit(false, err)
		} else {
			// The settlement's own failure is judged here, after the rollback, never inside the
			// transaction body: a damaged store is marked from outside the store it could not write
			// (halt.go, CRW-848), and the marker is not part of the transaction that rolled back.
			if d.halted(ctx, report, store.HaltSiteWrite, err) {
				return
			}
			report.Notes = append(report.Notes, "settlement rolled back for "+turn.TurnID+": "+err.Error())
			return
		}
	}
	if err != nil {
		if d.halted(ctx, report, store.HaltSiteWrite, err) {
			return
		}
		report.Notes = append(report.Notes, "settlement rolled back for "+turn.TurnID+": "+err.Error())
		return
	}
	switch {
	case ownEvent != "":
		report.Notes = append(report.Notes, "observation of "+turn.TurnID+" ("+turn.Status+") not asserted: the turn holds its own final child receipt "+ownEvent+", which is owed to the parent")
	case laterEvent != "":
		report.Notes = append(report.Notes, "observation of "+turn.TurnID+" ("+turn.Status+") not asserted: turn "+laterTurn+", admitted after it, holds the final child receipt "+laterEvent+", which is owed to the parent")
	}
	report.Observed++
}

// laterReceiptQuery finds the final, unsuppressed child receipt of a turn admitted after the observed
// turn in the same generation. "Admitted" is what the store's identity check accepts: a
// generation_turns row whose evidence names the generation's anchor. The observed turn is placed in
// that order by its own admission row; the anchor has none (it is the generation's immutable start),
// so every admitted turn comes after it. A turn that is neither matches nothing, and the observation
// path keeps refusing it as unassigned.
//
// The relationship and its generation are read here, not taken from the pass's earlier load, and the
// relationship must still be active: a generation opened or a relationship stopped since the load
// matches nothing, so DaemonObservation decides exactly as it did before. settle makes the lookup a second time
// inside the transaction that commits a silenced end, so the same holds between the first lookup and the commit.
//
// A final event is an accepted receipt, not a report that reached the parent. It counts only when a
// delivery (not marked superseded) or a delivery intent exists for it, because delivery is then owed
// through the scheduler, to the same parent and under the same holds an older event would meet. A final
// event with neither (one emitted with --no-enqueue, or left behind by a refused enqueue) leaves the
// parent unaware, so the end of the earlier turn is still reported. laterReceipt then asks, for a
// receipt not yet sent, whether it is still current: one a newer revision has replaced is never sent.
// A receipt that was already sent stands whatever replaced it, since the parent has its content.
const laterReceiptQuery = `SELECT t.turn_id, e.event_id, d.state AS delivery_state
FROM relationships rel
JOIN generations g ON g.relationship_id=rel.relationship_id AND g.execution_generation=rel.execution_generation
JOIN generation_turns t ON t.relationship_id=g.relationship_id AND t.execution_generation=g.execution_generation AND t.evidence=('explicit_admission_bound:' || g.dispatch_turn_id)
JOIN events e ON e.relationship_id=t.relationship_id AND e.execution_generation=t.execution_generation AND e.turn_thread_id=rel.child_task_id AND e.turn_id=t.turn_id
LEFT JOIN deliveries d ON d.event_id=e.event_id
WHERE rel.relationship_id=? AND rel.child_task_id=? AND rel.status='active' AND rel.superseded_by IS NULL
  AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>''
  AND t.turn_id<>?
  AND e.producer='child' AND e.stage='final' AND e.suppressed_reason IS NULL
  AND ((d.event_id IS NOT NULL AND d.state<>'superseded') OR EXISTS (SELECT 1 FROM delivery_intent i WHERE i.event_id=e.event_id))
  AND (g.dispatch_turn_id=? OR t.rowid>(SELECT o.rowid FROM generation_turns o WHERE o.relationship_id=g.relationship_id AND o.execution_generation=g.execution_generation AND o.turn_id=? AND o.evidence=('explicit_admission_bound:' || g.dispatch_turn_id)))
ORDER BY t.rowid, e.event_id`

// laterReceipt names a later admitted turn of the relationship's current generation that already
// holds a final, unsuppressed child event with a delivery owed to the parent, and that event; both are
// empty when there is none. Once a later turn has reported, the parent knows where the work stands and
// the end of this turn is not news (CRW-256), so settle asserts nothing for it.
func (d *Daemon) laterReceipt(ctx context.Context, relationshipID string, turn store.TurnReference) (laterTurn, event string, err error) {
	rows, err := d.Store.All(ctx, laterReceiptQuery, relationshipID, turn.ThreadID, turn.TurnID, turn.TurnID, turn.TurnID)
	if err != nil {
		return "", "", err
	}
	row, err := d.owedReceipt(ctx, rows)
	if err != nil || row == nil {
		return "", "", err
	}
	return row.Get("turn_id").(string), row.Get("event_id").(string), nil
}

// ownReceiptQuery finds the final, unsuppressed child receipt of the observed turn itself, in the
// relationship's current generation, while the parent is still owed its delivery. It answers the
// other half of the question laterReceiptQuery answers (CRW-668): a turn that already reported and
// then died is not news either, and the observation the daemon would synthesize for it is a second
// word on a turn the parent has already been told about, which would hide the child's own receipt
// from the decision path. The delivery-owed test is the same one laterReceiptQuery uses, and for the
// same reason: a final receipt owed nothing (emitted with --no-enqueue, or left behind by a refused
// enqueue) leaves the parent unaware, so the end of the turn is still reported.
//
// The observed turn is not required to be an admitted turn: intake refuses a receipt from a turn the
// generation does not hold, so a final child receipt on the observed turn already means the turn is
// the anchor or an admitted one. The relationship and its generation are read here, not taken from
// the pass's earlier load, and the settlement transaction asks again, so a generation opened or a
// relationship stopped since the load matches nothing and the end is judged again on the next pass.
const ownReceiptQuery = `SELECT e.event_id, d.state AS delivery_state
FROM relationships rel
JOIN generations g ON g.relationship_id=rel.relationship_id AND g.execution_generation=rel.execution_generation
JOIN events e ON e.relationship_id=rel.relationship_id AND e.execution_generation=g.execution_generation AND e.turn_thread_id=rel.child_task_id AND e.turn_id=?
LEFT JOIN deliveries d ON d.event_id=e.event_id
WHERE rel.relationship_id=? AND rel.child_task_id=? AND rel.status='active' AND rel.superseded_by IS NULL
  AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>''
  AND e.producer='child' AND e.stage='final' AND e.suppressed_reason IS NULL
  AND ((d.event_id IS NOT NULL AND d.state<>'superseded') OR EXISTS (SELECT 1 FROM delivery_intent i WHERE i.event_id=e.event_id))
ORDER BY e.first_seen_at DESC, e.rowid DESC`

// ownReceipt names the final, unsuppressed child receipt the observed turn itself holds while the
// parent is still owed its delivery, and "" when there is none.
func (d *Daemon) ownReceipt(ctx context.Context, relationshipID string, turn store.TurnReference) (string, error) {
	rows, err := d.Store.All(ctx, ownReceiptQuery, turn.TurnID, relationshipID, turn.ThreadID)
	if err != nil {
		return "", err
	}
	row, err := d.owedReceipt(ctx, rows)
	if err != nil || row == nil {
		return "", err
	}
	return row.Get("event_id").(string), nil
}

// owedReceipt picks the first of the rows whose receipt the parent is still owed, in the order the
// query gave them: a receipt already sent stands whatever replaced it, since the parent has its
// content, and any other counts only while no newer revision has replaced it.
func (d *Daemon) owedReceipt(ctx context.Context, rows []store.Row) (store.Row, error) {
	for _, row := range rows {
		candidate := row.Get("event_id").(string)
		if state, _ := row.Get("delivery_state").(string); state == delivery.Dispatched || state == delivery.Acknowledged || state == delivery.InboxOnly {
			return row, nil
		}
		replaced, err := d.Delivery.SupersessionReason(ctx, candidate)
		if err != nil {
			return nil, err
		}
		if replaced == "" {
			return row, nil
		}
	}
	return nil, nil
}

// statusText is the host turn status when it is a string; 28's lazy host
// decoding keeps the raw value, so any other shape matches no final status.
func statusText(turn *delivery.TurnInfo) (string, bool) {
	if turn == nil {
		return "", false
	}
	s, ok := turn.Status.(string)
	return s, ok
}
