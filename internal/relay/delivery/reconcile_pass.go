package delivery

import (
	"context"
	"database/sql"
	"strings"
)

// The daemon's reconciliation pass (daemon._reconcile, _attempts_for, _gate, _mark_gate,
// _reads_were_complete) with the reconciler's bounded listings (open_attempts, open_parents,
// open_attempt_count). The pass is reconciliation's; the tick that runs it is todo 29's.

// OpenAttemptsFor is open_attempts(limit, parents=[parent], offset).
func (rc *Reconciler) OpenAttemptsFor(ctx context.Context, parent string, limit, offset int) ([]Row, error) {
	query := "SELECT a.*, r.parent_task_id AS parent_task_id FROM attempts a JOIN deliveries d ON d.event_id = a.event_id JOIN relationships r ON r.relationship_id = d.relationship_id" + unresolvedWhere + " AND r.parent_task_id IN (?) ORDER BY a.observed_at LIMIT ?"
	args := []any{HeldUncertain, HeldUncertain, Sending, parent, limit}
	if offset > 0 {
		query += " OFFSET ?"
		args = append(args, offset)
	}
	return all(ctx, rc.Store, query, args...)
}

// OpenAttemptCount is open_attempt_count.
func (rc *Reconciler) OpenAttemptCount(ctx context.Context, parent string) (int, error) {
	row, err := one(ctx, rc.Store, "SELECT COUNT(*) AS c FROM attempts a JOIN deliveries d ON d.event_id = a.event_id JOIN relationships r ON r.relationship_id = d.relationship_id"+unresolvedWhere+" AND r.parent_task_id = ?", HeldUncertain, HeldUncertain, Sending, parent)
	if err != nil || row == nil {
		return 0, err
	}
	return int(row.I("c")), nil
}

// OpenParents is open_parents.
func (rc *Reconciler) OpenParents(ctx context.Context) ([]string, error) {
	rows, err := all(ctx, rc.Store, "SELECT DISTINCT r.parent_task_id AS parent_task_id FROM attempts a JOIN deliveries d ON d.event_id = a.event_id JOIN relationships r ON r.relationship_id = d.relationship_id"+unresolvedWhere+" ORDER BY r.parent_task_id", HeldUncertain, HeldUncertain, Sending)
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.S("parent_task_id"))
	}
	return out, err
}

// ReconcileReport is the reconciliation half of the daemon's TickReport.
type ReconcileReport struct {
	Reconciled, Skipped int
	Notes               []string
}

// ReconcilePass is daemon._reconcile: a budget dealt across parents from rotating cursors, each
// attempt reconciled only when its gate says there is something new to learn.
func ReconcilePass(ctx context.Context, rc *Reconciler, adapter Adapter, budget int, now float64, report *ReconcileReport) error {
	if budget == 0 {
		budget = 8
	}
	parents, err := rc.OpenParents(ctx)
	if err != nil || len(parents) == 0 {
		return err
	}
	cursors := &Scheduler{Delivery: rc.Delivery}
	cursor, err := cursors.cursor(ctx, "reconcile_parents", len(parents))
	if err != nil {
		return err
	}
	order := append(append([]string(nil), parents[cursor:]...), parents[:cursor]...)
	if err := cursors.advance(ctx, "reconcile_parents", 1, len(parents)); err != nil {
		return err
	}
	share := max(1, budget/len(order))
	queues := make([][]Row, len(order))
	for i, parent := range order {
		if queues[i], err = attemptsFor(ctx, rc, cursors, parent, share); err != nil {
			return err
		}
	}
	var dealt []Row
	for len(dealt) < budget {
		any := false
		for i := range queues {
			if len(dealt) >= budget {
				break
			}
			if len(queues[i]) > 0 {
				dealt = append(dealt, queues[i][0])
				queues[i] = queues[i][1:]
				any = true
			}
		}
		if !any {
			break
		}
	}
	for _, parent := range order {
		taken := 0
		for _, r := range dealt {
			if r.S("parent_task_id") == parent {
				taken++
			}
		}
		if taken > 0 {
			total, err := rc.OpenAttemptCount(ctx, parent)
			if err != nil {
				return err
			}
			if err := cursors.advance(ctx, "reconcile:"+parent, taken, total); err != nil {
				return err
			}
		}
	}
	for _, attempt := range dealt {
		id := attempt.S("request_id")
		decision, fingerprint, err := gate(ctx, rc, adapter, attempt)
		if err != nil {
			return err
		}
		if !decision {
			report.Skipped++
			continue
		}
		outcome, err := rc.ReconcileAttempt(ctx, id, adapter, &now)
		if err != nil {
			text := err.Error()
			if err := markGate(ctx, rc, id, nil, true, text); err != nil {
				return err
			}
			report.Notes = append(report.Notes, "reconcile failed for "+id+": "+text)
			continue
		}
		complete := readsWereComplete(outcome)
		var print any
		var failure any
		if complete {
			print = fingerprint
		} else {
			failure = "reads incomplete"
		}
		if err := markGate(ctx, rc, id, print, !complete, failure); err != nil {
			return err
		}
		report.Reconciled++
	}
	return nil
}

func attemptsFor(ctx context.Context, rc *Reconciler, cursors *Scheduler, parent string, share int) ([]Row, error) {
	total, err := rc.OpenAttemptCount(ctx, parent)
	if err != nil || total == 0 {
		return nil, err
	}
	want := min(share, total)
	start, err := cursors.cursor(ctx, "reconcile:"+parent, total)
	if err != nil {
		return nil, err
	}
	taken, err := rc.OpenAttemptsFor(ctx, parent, want, start)
	if err != nil {
		return nil, err
	}
	if len(taken) < want {
		seen := map[string]bool{}
		for _, r := range taken {
			seen[r.S("request_id")] = true
		}
		again, err := rc.OpenAttemptsFor(ctx, parent, want, 0)
		if err != nil {
			return nil, err
		}
		for _, r := range again {
			if len(taken) >= want {
				break
			}
			if !seen[r.S("request_id")] {
				taken = append(taken, r)
			}
		}
	}
	return taken, nil
}

func readsWereComplete(outcome Obj) bool {
	if v, _ := get(outcome, "changed"); truthy(v) {
		return false
	}
	if trace, ok := get(outcome, "recipientTrace"); ok {
		if o, _ := trace.(Obj); o != nil {
			if p, _ := get(o, "pending"); truthy(p) {
				return false
			}
		}
	}
	observation, scan := str(outcome, "operationObservation"), str(outcome, "recipientScan")
	if strings.Contains(observation, "unreadable") || strings.Contains(scan, "unreadable") {
		return false
	}
	if strings.HasPrefix(scan, "not scanned") {
		return str(outcome, "evidence") != NoEvidence
	}
	return true
}

// gate is daemon._gate: is there anything new to learn? The fingerprint is read first.
func gate(ctx context.Context, rc *Reconciler, adapter Adapter, attempt Row) (bool, any, error) {
	id := attempt.S("request_id")
	row, err := one(ctx, rc.Store, "SELECT * FROM reconcile_gate WHERE request_id = ?", id)
	if err != nil {
		return false, nil, err
	}
	delivery, err := rc.Delivery.Find(ctx, attempt.S("event_id"))
	if err != nil {
		return false, nil, err
	}
	receipt, readErr := adapter.GetOperation(id)
	var content string
	if readErr == nil {
		content, readErr = adapter.RecipientFingerprint(delivery.S("recipient_thread_id"))
	}
	if readErr != nil {
		return true, nil, markGate(ctx, rc, id, nil, true, readErr.Error())
	}
	status, turnID := "missing", ""
	if receipt != nil {
		if v, ok := get(receipt, "status"); ok {
			status = pyStr(v)
		}
		if v, ok := get(receipt, "turnId"); ok {
			if turn, ok := usableTurnID(v).(string); ok {
				turnID = turn
			}
		}
	}
	fingerprint := status + "|" + turnID + "|" + content
	if row == nil || row.I("retry_required") != 0 {
		return true, fingerprint, nil
	}
	if strings.HasPrefix(attempt.S("recipient_scan"), unknownMarkPrefix) {
		updated, ok := epoch(row.S("updated_at"))
		if !ok || rc.Clock.Now()-updated >= UndecidedRecheckSeconds {
			return true, fingerprint, nil
		}
	}
	return fingerprint != row.S("fingerprint"), fingerprint, nil
}

func markGate(ctx context.Context, rc *Reconciler, requestID string, fingerprint any, retry bool, failure any) error {
	return rc.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		_, err := execSQL(ctx, rc.Store, "INSERT INTO reconcile_gate (request_id, fingerprint, retry_required, last_error, updated_at) VALUES (?,?,?,?,?) ON CONFLICT(request_id) DO UPDATE SET fingerprint=COALESCE(excluded.fingerprint, reconcile_gate.fingerprint), retry_required=excluded.retry_required, last_error=excluded.last_error, updated_at=excluded.updated_at",
			requestID, fingerprint, boolFlag(retry), failure, rc.Clock.ISO())
		return err
	})
}
