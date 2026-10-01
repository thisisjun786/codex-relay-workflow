package daemon

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func (d *Daemon) cursor(ctx context.Context, name string, size int) (int, error) {
	if size <= 0 {
		return 0, nil
	}
	var v string
	err := d.Store.Q(ctx).QueryRowContext(ctx, "SELECT cursor FROM discovery_cursors WHERE task_id='scheduler' AND listing=?", name).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	n, _ := strconv.Atoi(v)
	return ((n % size) + size) % size, nil
}
func (d *Daemon) saveCursor(ctx context.Context, name, value string) error {
	return d.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		_, err := d.Store.Q(tx).ExecContext(tx, "INSERT INTO discovery_cursors (task_id,listing,cursor,updated_at) VALUES ('scheduler',?,?,?) ON CONFLICT(task_id,listing) DO UPDATE SET cursor=excluded.cursor,updated_at=excluded.updated_at", name, value, d.Clock.ISO())
		return err
	})
}
func (d *Daemon) advance(ctx context.Context, name string, by, size int) error {
	if size <= 0 {
		return nil
	}
	n, err := d.cursor(ctx, name, size)
	if err != nil {
		return err
	}
	return d.saveCursor(ctx, name, strconv.Itoa((n+max(1, by))%size))
}
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
func (d *Daemon) turns(ctx context.Context, r delivery.Relationship, share int) ([]string, error) {
	current := ""
	history := []string{}
	for _, g := range r.Generations {
		turn := g.S("dispatch_turn_id")
		if turn == "" {
			continue
		}
		if g.I("execution_generation") == r.Generation {
			current = turn
		} else {
			history = append(history, turn)
		}
	}
	staged, err := d.Store.All(ctx, "SELECT turn_id FROM events WHERE stage='staged' AND turn_thread_id=? AND relationship_id=? ORDER BY first_seen_at", r.Child.TaskID, r.ID)
	if err != nil {
		return nil, err
	}
	var saved string
	err = d.Store.Q(ctx).QueryRowContext(ctx, "SELECT cursor FROM discovery_cursors WHERE task_id='scheduler' AND listing=?", "admitted:"+r.ID).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	var pos struct{ After, Through int64 }
	_ = json.Unmarshal([]byte(saved), &pos)
	if pos.After >= pos.Through {
		pos.After = 0
		if err = d.Store.Q(ctx).QueryRowContext(ctx, "SELECT COALESCE(MAX(rowid),0) FROM generation_turns").Scan(&pos.Through); err != nil {
			return nil, err
		}
	}
	query := "SELECT t.rowid AS admission_row,t.turn_id,CASE WHEN t.relationship_id=? THEN (EXISTS (SELECT 1 FROM generations g WHERE g.relationship_id=t.relationship_id AND g.execution_generation=t.execution_generation AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>'' AND t.evidence=('explicit_admission_bound:' || g.dispatch_turn_id)) AND NOT EXISTS (SELECT 1 FROM assignment_settlements s WHERE s.relationship_id=t.relationship_id AND s.thread_id=? AND s.turn_id=t.turn_id)) ELSE 0 END AS eligible FROM generation_turns t WHERE t.rowid>? AND t.rowid<=? ORDER BY t.rowid LIMIT ?"
	page, err := d.Store.All(ctx, query, r.ID, r.Child.TaskID, pos.After, pos.Through, max(1, share-1))
	if err != nil {
		return nil, err
	}
	if len(page) == 0 && pos.After != 0 {
		page, err = d.Store.All(ctx, query, r.ID, r.Child.TaskID, 0, pos.Through, max(1, share-1))
		if err != nil {
			return nil, err
		}
	}
	candidates := []string{}
	for _, row := range staged {
		candidates = append(candidates, row.Get("turn_id").(string))
	}
	for _, row := range page {
		if row.Get("eligible").(int64) != 0 {
			candidates = append(candidates, row.Get("turn_id").(string))
		}
	}
	candidates = append(candidates, history...)
	ring := []string{}
	for _, turn := range candidates {
		if turn == current || slices.Contains(ring, turn) {
			continue
		}
		worth, e := d.worth(ctx, r.Child.TaskID, turn, r.ID)
		if e != nil {
			return nil, e
		}
		if worth {
			ring = append(ring, turn)
		}
	}
	selected := []string{}
	if current != "" {
		worth, e := d.worth(ctx, r.Child.TaskID, current, r.ID)
		if e != nil {
			return nil, e
		}
		if worth {
			selected = append(selected, current)
		}
	}
	remaining := share - len(selected)
	if remaining <= 0 && len(ring) > 0 {
		alt, e := d.cursor(ctx, "alt:"+r.ID, 2)
		if e != nil {
			return nil, e
		}
		if e = d.advance(ctx, "alt:"+r.ID, 1, 2); e != nil {
			return nil, e
		}
		if alt == 1 {
			selected = nil
			remaining = 1
		}
	}
	if remaining > 0 && len(ring) > 0 {
		taken := min(remaining, len(ring))
		start, e := d.cursor(ctx, "ring:"+r.ID, len(ring))
		if e != nil {
			return nil, e
		}
		for i := 0; i < taken; i++ {
			selected = append(selected, ring[(start+i)%len(ring)])
		}
		if e = d.advance(ctx, "ring:"+r.ID, taken, len(ring)); e != nil {
			return nil, e
		}
	}
	var consumed int64
	for _, row := range page {
		turn := row.Get("turn_id").(string)
		if row.Get("eligible").(int64) != 0 && turn != current && !slices.Contains(selected, turn) {
			break
		}
		consumed = row.Get("admission_row").(int64)
	}
	if consumed != 0 {
		if err = d.saveCursor(ctx, "admitted:"+r.ID, fmt.Sprintf("{\"after\": %d, \"through\": %d}", consumed, pos.Through)); err != nil {
			return nil, err
		}
	}
	return selected, nil
}
func (d *Daemon) observe(ctx context.Context, report *Report) error {
	rows, err := d.Store.All(ctx, "SELECT relationship_id FROM relationships WHERE status='active' AND superseded_by IS NULL")
	if err != nil || len(rows) == 0 {
		return err
	}
	relations := []delivery.Relationship{}
	for _, row := range rows {
		r, e := delivery.LoadRelationship(ctx, d.Store, row.Get("relationship_id").(string))
		if e != nil {
			return e
		}
		relations = append(relations, r)
	}
	budget := d.Policy.MaxTurnReads
	served := max(1, min(len(relations), budget/max(1, d.Policy.MinRelationshipShare)))
	start, err := d.cursor(ctx, "relationships", len(relations))
	if err != nil {
		return err
	}
	share := max(1, budget/served)
	reads := 0
	for i := 0; i < served; i++ {
		r := relations[(start+i)%len(relations)]
		turns, e := d.turns(ctx, r, share)
		if e != nil {
			return e
		}
		for _, turnID := range turns {
			if reads >= budget {
				break
			}
			reads++
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
				continue
			}
			worth, e := d.worth(ctx, r.Child.TaskID, turnID, r.ID)
			if e != nil {
				return e
			}
			if !worth {
				continue
			}
			d.settle(ctx, r, store.TurnReference{ThreadID: r.Child.TaskID, TurnID: turnID, Status: final}, report)
		}
	}
	return d.advance(ctx, "relationships", served, len(relations))
}
func (d *Daemon) settle(ctx context.Context, r delivery.Relationship, turn store.TurnReference, report *Report) {
	synthesized, laterTurn, laterEvent := "", "", ""
	ended := turn.Status == "failed" || turn.Status == "interrupted"
	if ended {
		var err error
		if laterTurn, laterEvent, err = d.laterReceipt(ctx, r, turn); err != nil {
			report.Notes = append(report.Notes, "later receipt lookup failed for "+turn.TurnID+": "+err.Error())
			return
		}
	}
	if laterEvent != "" {
		// The parent already heard from a later turn of this generation, so the end of this one is
		// not news. The turn is still settled below, with no event, so it is not read again.
		report.Notes = append(report.Notes, "observation of "+turn.TurnID+" ("+turn.Status+") not asserted: turn "+laterTurn+", admitted after it, already holds the final child receipt "+laterEvent)
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
const laterReceiptQuery = `SELECT t.turn_id, e.event_id
FROM generations g
JOIN generation_turns t ON t.relationship_id=g.relationship_id AND t.execution_generation=g.execution_generation AND t.evidence=('explicit_admission_bound:' || g.dispatch_turn_id)
JOIN events e ON e.relationship_id=t.relationship_id AND e.execution_generation=t.execution_generation AND e.turn_thread_id=? AND e.turn_id=t.turn_id
WHERE g.relationship_id=? AND g.execution_generation=? AND g.dispatch_turn_id IS NOT NULL AND g.dispatch_turn_id<>''
  AND t.turn_id<>?
  AND e.producer='child' AND e.stage='final' AND e.suppressed_reason IS NULL
  AND (g.dispatch_turn_id=? OR t.rowid>(SELECT o.rowid FROM generation_turns o WHERE o.relationship_id=g.relationship_id AND o.execution_generation=g.execution_generation AND o.turn_id=? AND o.evidence=('explicit_admission_bound:' || g.dispatch_turn_id)))
ORDER BY t.rowid, e.event_id LIMIT 1`

// laterReceipt names a later admitted turn of the relationship's current generation that already
// holds a final, unsuppressed child event, and that event; both are empty when there is none. Once a
// later turn has reported, the parent knows where the work stands and the end of this turn is not
// news (CRW-256), so settle asserts nothing for it.
func (d *Daemon) laterReceipt(ctx context.Context, r delivery.Relationship, turn store.TurnReference) (laterTurn, event string, err error) {
	row, err := d.Store.One(ctx, laterReceiptQuery, turn.ThreadID, r.ID, r.Generation, turn.TurnID, turn.TurnID, turn.TurnID)
	if err != nil || row == nil {
		return "", "", err
	}
	return row.Get("turn_id").(string), row.Get("event_id").(string), nil
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
