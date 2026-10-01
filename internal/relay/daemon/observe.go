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
	d         *Daemon
	report    *Report
	start     time.Time
	floor     int // reads made whatever the clock says
	reads     int
	stopped   bool
	relations map[string]delivery.Relationship
	used      [2]map[string]int // reads this tick, by class and relationship
}

// spent reports whether the pass has used the time Policy.MaxObserveSeconds allows it, and says so once. The
// first reads, one for each class that waits, are made whatever the clock says, so a slow class cannot keep the
// other out of the tick.
func (o *pass) spent() bool {
	if limit := o.d.Policy.MaxObserveSeconds; !o.stopped && limit > 0 && o.reads >= o.floor {
		if took := o.d.monotonic().Sub(o.start).Seconds(); took >= limit {
			o.stopped = true
			o.report.Notes = append(o.report.Notes, fmt.Sprintf("observation stopped after %.1f s, the limit for one pass; the other turns wait for the next tick", took))
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
		if err = d.read(ctx, r, next.t.id, report); err != nil {
			return err
		}
	}
	return nil
}

// read asks the host about one turn of r, records the attempt and, when the turn ended, settles it.
func (d *Daemon) read(ctx context.Context, r delivery.Relationship, turnID string, report *Report) (err error) {
	turn, e := d.Host.ReadTurn(r.Child.TaskID, turnID)
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
		return err
	}
	// Python: turn.status not in ("completed", "failed", "interrupted"); a
	// non-string host status equals none of them.
	final, _ := statusText(turn)
	if e != nil || turn == nil || !slices.Contains([]string{"completed", "failed", "interrupted"}, final) {
		return nil
	}
	worth, e := d.worth(ctx, r.Child.TaskID, turnID, r.ID)
	if e != nil {
		return e
	}
	if worth {
		d.settle(ctx, r, store.TurnReference{ThreadID: r.Child.TaskID, TurnID: turnID, Status: final}, report)
	}
	return nil
}

func (d *Daemon) settle(ctx context.Context, r delivery.Relationship, turn store.TurnReference, report *Report) {
	synthesized, laterTurn, laterEvent := "", "", ""
	ended := turn.Status == "failed" || turn.Status == "interrupted"
	if ended {
		var err error
		if laterTurn, laterEvent, err = d.laterReceipt(ctx, r.ID, turn); err != nil {
			report.Notes = append(report.Notes, "later receipt lookup failed for "+turn.TurnID+": "+err.Error())
			return
		}
	}
	if laterEvent != "" {
		// A later turn of this generation holds a final child receipt whose delivery the parent is
		// owed, so the end of this one is not news. The turn is still settled below, with no event, so
		// it is not read again.
		report.Notes = append(report.Notes, "observation of "+turn.TurnID+" ("+turn.Status+") not asserted: turn "+laterTurn+", admitted after it, holds the final child receipt "+laterEvent+", which is owed to the parent")
	} else if ended {
		receipt, err := d.Intake.DaemonObservation(ctx, r.ID, turn)
		if err != nil {
			if store.RefusalReason(err) == store.ReasonRelationshipNotActive {
				report.Notes = append(report.Notes, "observation deferred, "+r.ID+" is not active: "+err.Error())
				return
			}
			if store.RefusalReason(err) != "" {
				report.Notes = append(report.Notes, "daemon observation refused: "+err.Error())
			} else {
				report.Notes = append(report.Notes, "daemon observation failed: "+err.Error())
				return
			}
		} else {
			synthesized = receipt.EventID
		}
	}
	commit := func(queue bool, refusal error) error {
		return d.Store.Compose(ctx, func(tx context.Context, conn *sql.Conn) error {
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
			if laterEvent != "" {
				detail := pyjson.Dumps(contract.OrderedObject{{Key: "status", Value: turn.Status}, {Key: "laterTurn", Value: laterTurn}, {Key: "laterEvent", Value: laterEvent}}, pyjson.Options{})
				if _, err = conn.ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES (?,'observation_not_asserted',?,?)", now, turn.TurnID, detail); err != nil {
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
	err := commit(true, nil)
	if err != nil {
		var refused *store.RefusedError
		if errors.As(err, &refused) {
			report.Notes = append(report.Notes, "enqueue refused for "+turn.TurnID+": "+err.Error())
			err = commit(false, err)
		} else {
			report.Notes = append(report.Notes, "settlement rolled back for "+turn.TurnID+": "+err.Error())
			return
		}
	}
	if err != nil {
		report.Notes = append(report.Notes, "settlement rolled back for "+turn.TurnID+": "+err.Error())
		return
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
// matches nothing, so DaemonObservation decides exactly as it did before.
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
	for _, row := range rows {
		candidate := row.Get("event_id").(string)
		if state, _ := row.Get("delivery_state").(string); state == delivery.Dispatched || state == delivery.Acknowledged || state == delivery.InboxOnly {
			return row.Get("turn_id").(string), candidate, nil
		}
		replaced, err := d.Delivery.SupersessionReason(ctx, candidate)
		if err != nil {
			return "", "", err
		}
		if replaced == "" {
			return row.Get("turn_id").(string), candidate, nil
		}
	}
	return "", "", nil
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
