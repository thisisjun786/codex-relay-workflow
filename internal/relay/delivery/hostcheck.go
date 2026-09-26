package delivery

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The host-loss half of reconciliation (reconcile.py _settle_confirmed, check_dispatched_turn,
// confirm_delivery) and the daemon's recipient-turn pass (daemon._check_dispatched_turns). The
// rest of the tick is todo 29's; Turns is the state this pass keeps between ticks.

// settleConfirmed is _settle_confirmed: an uncertain send already confirmed from its token stays
// confirmed, and what is left to ask is whether the parent still has that turn.
func (rc *Reconciler) settleConfirmed(ctx context.Context, attempt, delivery Row, observation string, adapter Adapter) (Obj, error) {
	record := Obj{}
	if !attempt.N("record") {
		record = loadsObj(attempt.S("record"))
	}
	turnID, _ := get(record, "turnId")
	if !truthy(turnID) {
		turnID = delivery.Opt("dispatch_turn_id")
	}
	reading := ReadRecipientTurn(adapter, rc.Clock, attempt, delivery, turnID)
	out := Obj{{Key: "evidence", Value: TurnFound}, {Key: "state", Value: attempt.Opt("state")}, {Key: "operationObservation", Value: observation}, {Key: "record", Value: record},
		{Key: "recipientTurn", Value: reading}, {Key: "detail", Value: "already confirmed from its token in the recipient's items; not scanned again"}}
	return rc.afterReading(ctx, out, attempt, delivery, reading, observation)
}

// afterReading records what a recipient-turn reading decided: a loss (completion) or a report
// only (revision), otherwise the undecided name.
func (rc *Reconciler) afterReading(ctx context.Context, out Obj, attempt, delivery Row, reading Reading, observation any) (Obj, error) {
	switch {
	case str(reading, "finding") == HostLostTurn && delivery.S("kind") == Completion:
		loss, err := SettleLoss(ctx, rc.Store, rc.Clock, attempt.S("request_id"), reading, observation)
		if err != nil {
			return nil, err
		}
		for _, f := range loss {
			out = set(out, f.Key, f.Value)
		}
	case str(reading, "finding") == HostLostTurn:
		out = set(out, "redelivery", ReportOnly)
	case delivery.S("kind") == Completion:
		if _, err := RecordUndecided(ctx, rc.Store, attempt.S("request_id"), reading); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CheckDispatchedTurn is check_dispatched_turn: the daemon's question for one delivered
// completion. Writes nothing unless the host lost the turn (or an undecided name changes).
func (rc *Reconciler) CheckDispatchedTurn(ctx context.Context, requestID string, adapter Adapter) (Obj, error) {
	attempt, err := one(ctx, rc.Store, "SELECT * FROM attempts WHERE request_id = ?", requestID)
	if err != nil {
		return nil, err
	}
	if attempt == nil {
		return nil, fmt.Errorf("KeyError: %s", store.PyRepr(requestID))
	}
	delivery, err := rc.Delivery.Get(ctx, attempt.S("event_id"))
	if err != nil {
		return nil, err
	}
	record := Obj{}
	if !attempt.N("record") {
		record = loadsObj(attempt.S("record"))
	}
	turnID, _ := get(record, "turnId")
	if !truthy(turnID) {
		turnID = delivery.Opt("dispatch_turn_id")
	}
	reading := ReadRecipientTurn(adapter, rc.Clock, attempt, delivery, turnID)
	out := Obj{{Key: "eventId", Value: attempt.S("event_id")}, {Key: "requestId", Value: requestID}, {Key: "state", Value: attempt.Opt("state")}, {Key: "recipientTurn", Value: reading}}
	if str(reading, "finding") == HostLostTurn {
		loss, err := SettleLoss(ctx, rc.Store, rc.Clock, requestID, reading, nil)
		if err != nil {
			return nil, err
		}
		for _, f := range loss {
			out = set(out, f.Key, f.Value)
		}
		return out, nil
	}
	changed, err := RecordUndecided(ctx, rc.Store, requestID, reading)
	if err != nil {
		return nil, err
	}
	return append(out, F{Key: "undecidedChanged", Value: changed}), nil
}

// ConfirmDelivery is confirm_delivery: settle an uncertain completion send before the parent's
// acknowledgement is judged, reading the acknowledging turn's own items for the message. Nil when
// nothing was read; read errors are reported in the outcome, never returned.
func (rc *Reconciler) ConfirmDelivery(ctx context.Context, eventID string, adapter Adapter, turnID string) (Obj, error) {
	delivery, err := rc.Delivery.Find(ctx, eventID)
	if err != nil || delivery == nil || delivery.S("kind") != Completion || delivery.S("state") != HeldUncertain {
		return nil, err
	}
	current, err := one(ctx, rc.Store, "SELECT request_id FROM attempts WHERE event_id = ? AND attempt_no = ?", eventID, delivery.I("attempt_count"))
	if err != nil || current == nil {
		return nil, err
	}
	requestID := current.S("request_id")
	out := Obj{{Key: "eventId", Value: eventID}, {Key: "requestId", Value: requestID}}
	reconciled, err := rc.ReconcileAttempt(ctx, requestID, adapter, nil)
	if err != nil {
		return append(out, F{Key: "error", Value: "reconcile: " + errorLabel(err)}), nil
	}
	kept := Obj{}
	for _, f := range reconciled {
		if f.Key != "record" {
			kept = append(kept, f)
		}
	}
	out = append(out, F{Key: "reconciled", Value: kept})
	if turnID == "" {
		return out, nil
	}
	if delivery, err = rc.Delivery.Find(ctx, eventID); err != nil {
		return nil, err
	}
	attempt, err := one(ctx, rc.Store, "SELECT * FROM attempts WHERE request_id = ?", requestID)
	if err != nil {
		return nil, err
	}
	if delivery == nil || delivery.S("state") != HeldUncertain || delivery.I("attempt_count") != attempt.I("attempt_no") || attempt.S("internal_state") != "settled" || attempt.S("state") != HeldUncertain {
		return out, nil
	}
	scan, err := adapter.FindTokenInTurn(delivery.S("recipient_thread_id"), requestID, turnID, InTurnItemsMax)
	if err != nil {
		return append(out, F{Key: "turnRead", Value: "unreadable: " + errorLabel(err)}), nil
	}
	detail := fmt.Sprintf("found=%s in turn %s, the acknowledging turn (%d of its items read)", pyStr(scan.Found), turnID, scan.Scanned)
	out = append(out, F{Key: "turnRead", Value: detail})
	if !scan.Found {
		return out, nil
	}
	observation := str(reconciled, "operationObservation")
	if observation == "" {
		observation = attempt.S("operation_observation")
	}
	settled, err := rc.settleFromScan(ctx, attempt, delivery, scan, observation, detail, rc.Clock.Now())
	switch {
	case isRace(err):
		name := "_AttemptChanged"
		if errors.As(err, &attemptLost{}) {
			name = "_AttemptLost"
		}
		return append(out, F{Key: "confirmed", Value: nil}, F{Key: "detail", Value: "the attempt was settled elsewhere first (" + name + ")"}), nil
	case err != nil:
		return nil, err
	}
	confirmed := Obj{}
	for _, f := range settled {
		if f.Key != "record" {
			confirmed = append(confirmed, f)
		}
	}
	return append(out, F{Key: "confirmed", Value: confirmed}), nil
}

func isRace(err error) bool {
	var changed attemptChanged
	return errors.As(err, &attemptLost{}) || errors.As(err, &changed)
}

// awaitingSQL is hostloss._AWAITING_SQL: delivered completions still owed an acknowledgement.
const awaitingSQL = "SELECT d.event_id, a.request_id FROM deliveries d JOIN attempts a ON a.event_id = d.event_id AND a.attempt_no = d.attempt_count WHERE d.kind = ? AND d.state = ? AND d.hold_reason IS NULL AND a.internal_state = 'settled' AND a.state IN (?, ?) AND NOT EXISTS (SELECT 1 FROM acks k WHERE k.event_id = d.event_id AND (k.verified = 'verified' OR a.sent_at IS NULL OR k.ack_at >= a.sent_at))"

// AwaitingAck is hostloss.awaiting_ack: one page, keyed by event.
func AwaitingAck(ctx context.Context, s *store.Store, after string, limit int) ([]Row, error) {
	query, args := awaitingSQL, []any{Completion, Dispatched, Dispatched, HeldUncertain}
	if after != "" {
		query += " AND d.event_id > ?"
		args = append(args, after)
	}
	return all(ctx, s, query+" ORDER BY d.event_id LIMIT ?", append(args, limit)...)
}

// StillAwaiting is hostloss.still_awaiting.
func StillAwaiting(ctx context.Context, s *store.Store, requestIDs []string) (map[string]bool, error) {
	live := map[string]bool{}
	if len(requestIDs) == 0 {
		return live, nil
	}
	args := []any{Completion, Dispatched, Dispatched, HeldUncertain}
	for _, id := range requestIDs {
		args = append(args, id)
	}
	rows, err := all(ctx, s, awaitingSQL+" AND a.request_id IN ("+strings.TrimSuffix(strings.Repeat("?, ", len(requestIDs)), ", ")+")", args...)
	for _, r := range rows {
		live[r.S("request_id")] = true
	}
	return live, err
}

// UndecidedRecheckSeconds is daemon.UNDECIDED_RECHECK_SECONDS.
const UndecidedRecheckSeconds = 600.0

// TurnCheckReport is the host-loss half of the daemon's TickReport.
type TurnCheckReport struct {
	TurnsLost, TurnsUndecided int
	Notes                     []string
}

// TurnChecks is daemon._check_dispatched_turns with its in-memory state: where the rotation
// resumes, the finished turns not read again, and the undecided readings' next read.
type TurnChecks struct {
	Reconciler *Reconciler
	Budget     int
	Page       int
	after      string
	settled    []string
	undecided  []string
	nextRead   map[string]float64
}

// Settled is the finished-turn memory, oldest checked first (daemon._turns_settled).
func (tc *TurnChecks) Settled() []string { return append([]string(nil), tc.settled...) }

func remove(list []string, id string) []string {
	for i, v := range list {
		if v == id {
			return append(list[:i:i], list[i+1:]...)
		}
	}
	return list
}

func (tc *TurnChecks) forgetDeparted(ctx context.Context, page int) error {
	batch := append([]string(nil), tc.settled[:min(page, len(tc.settled))]...)
	batch = append(batch, tc.undecided[:min(page, len(tc.undecided))]...)
	if len(batch) == 0 {
		return nil
	}
	live, err := StillAwaiting(ctx, tc.Reconciler.Store, batch)
	if err != nil {
		return err
	}
	for _, cache := range []*[]string{&tc.settled, &tc.undecided} {
		for _, id := range batch {
			if contains(*cache, id) {
				*cache = remove(*cache, id)
				if live[id] {
					*cache = append(*cache, id)
				}
			}
		}
	}
	for id := range tc.nextRead {
		if !contains(tc.undecided, id) {
			delete(tc.nextRead, id)
		}
	}
	return nil
}

func contains(list []string, id string) bool {
	for _, v := range list {
		if v == id {
			return true
		}
	}
	return false
}

// Pass is one tick's recipient-turn check.
func (tc *TurnChecks) Pass(ctx context.Context, adapter Adapter, now float64, report *TurnCheckReport) {
	budget := tc.Budget
	if budget <= 0 || adapter == nil {
		return
	}
	if tc.nextRead == nil {
		tc.nextRead = map[string]float64{}
	}
	page := tc.Page
	if page == 0 {
		page = max(64, budget*16)
	}
	after := tc.after
	if err := tc.forgetDeparted(ctx, page); err != nil {
		report.Notes = append(report.Notes, "recipient turn check could not list deliveries: "+err.Error())
		return
	}
	rows, err := AwaitingAck(ctx, tc.Reconciler.Store, after, page)
	if err == nil && after != "" && len(rows) < page {
		var head []Row
		if head, err = AwaitingAck(ctx, tc.Reconciler.Store, "", page); err == nil {
			for _, r := range head {
				if r.S("event_id") <= after {
					rows = append(rows, r)
				}
			}
		}
	}
	if err != nil {
		report.Notes = append(report.Notes, "recipient turn check could not list deliveries: "+err.Error())
		return
	}
	spent := 0
	for _, row := range rows {
		if spent >= budget {
			break
		}
		tc.after = row.S("event_id")
		id := row.S("request_id")
		if contains(tc.settled, id) {
			continue
		}
		if at, ok := tc.nextRead[id]; ok && contains(tc.undecided, id) && at > now {
			continue
		}
		spent++
		outcome, err := tc.Reconciler.CheckDispatchedTurn(ctx, id, adapter)
		if err != nil {
			report.Notes = append(report.Notes, fmt.Sprintf("recipient turn check failed for %s: %s", id, err))
			continue
		}
		v, _ := get(outcome, "recipientTurn")
		reading := v.(Obj)
		if u, _ := get(reading, "undecided"); truthy(u) {
			if !contains(tc.undecided, id) {
				tc.undecided = append(tc.undecided, id)
			}
			tc.nextRead[id] = now + UndecidedRecheckSeconds
		} else {
			tc.undecided = remove(tc.undecided, id)
			delete(tc.nextRead, id)
		}
		status, _ := get(reading, "status")
		detail := str(reading, "detail")
		if str(reading, "finding") == Present && contains(terminalTurn, pyStrOrEmpty(status)) {
			if !contains(tc.settled, id) {
				tc.settled = append(tc.settled, id)
			}
		} else if str(reading, "finding") == Unknown && strings.HasPrefix(detail, "unreadable") {
			report.Notes = append(report.Notes, fmt.Sprintf("recipient turn unreadable for %s: %s", id, detail))
		}
		if r := str(outcome, "redelivery"); r == Requeued || r == HeldRedelivery {
			report.TurnsLost++
		}
		if n, ok := get(outcome, "undecidedChanged"); ok {
			report.TurnsUndecided += int(n.(int64))
		}
	}
}

// KeptUnconfirmed is kept_unconfirmed: (event, acknowledging turn) of each acknowledgement kept
// while its delivery is still unconfirmed and whose check is due.
func (a *Ack) KeptUnconfirmed(ctx context.Context, now float64, limit int) ([][2]string, error) {
	rows, err := all(ctx, a.Store, "SELECT a.event_id, a.ack_turn_id FROM acks a JOIN ack_evidence e ON e.event_id = a.event_id JOIN deliveries d ON d.event_id = a.event_id WHERE a.verified = 'unverified_turn' AND e.last_reason = ? AND d.state = ? AND (e.next_check_at IS NULL OR e.next_check_at <= ?) ORDER BY COALESCE(e.next_check_at, 0), a.event_id LIMIT ?",
		DeliveryUnconfirmed, HeldUncertain, now, limit)
	out := make([][2]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, [2]string{r.S("event_id"), r.S("ack_turn_id")})
	}
	return out, err
}

func (a *Ack) kept(ctx context.Context, eventID string) (Row, error) {
	return one(ctx, a.Store, "SELECT a.event_id, a.ack_turn_id, a.ack_at, a.accepted, COALESCE(e.attempts, 0) AS attempts, e.last_reason, e.fingerprint, e.next_check_at FROM acks a JOIN ack_evidence e ON e.event_id = a.event_id WHERE a.event_id = ? AND a.verified = 'unverified_turn' AND e.last_reason = ?", eventID, DeliveryUnconfirmed)
}

// KeptTurn is kept_turn: the acknowledging turn of a kept acknowledgement, or "".
func (a *Ack) KeptTurn(ctx context.Context, eventID string) (string, error) {
	row, err := a.kept(ctx, eventID)
	if err != nil || row == nil {
		return "", err
	}
	return row.S("ack_turn_id"), nil
}

// CompletePending is complete_pending: one kept acknowledgement completed now, as the pending
// pass would. Nil when nothing is kept for this event.
func (a *Ack) CompletePending(ctx context.Context, eventID string, adapter Adapter) (Obj, error) {
	now := a.Clock.Now()
	pending, err := a.kept(ctx, eventID)
	if err != nil || pending == nil {
		return nil, err
	}
	row, err := a.Delivery.Find(ctx, eventID)
	if err != nil || row == nil {
		return nil, err
	}
	verification, err := a.verifyAckTurn(ctx, row, pending.S("ack_turn_id"), adapter)
	if err != nil {
		if Reason(err) == "" {
			return nil, err
		}
		verification = Reason(err)
	}
	return a.settlePendingAck(ctx, eventID, pending, verification, now, row)
}

// ConfirmKeptAcks is daemon._confirm_kept_acks.
func ConfirmKeptAcks(ctx context.Context, a *Ack, rc *Reconciler, adapter Adapter, now float64) []string {
	var notes []string
	rows, err := a.KeptUnconfirmed(ctx, now, 8)
	if err != nil {
		return []string{"kept acknowledgement pass failed: " + err.Error()}
	}
	for _, r := range rows {
		outcome, err := rc.ConfirmDelivery(ctx, r[0], adapter, r[1])
		if err != nil {
			notes = append(notes, fmt.Sprintf("kept acknowledgement %s not confirmed: %s", r[0], err))
			continue
		}
		problem := str(outcome, "error")
		if read := str(outcome, "turnRead"); problem == "" && strings.HasPrefix(read, "unreadable") {
			problem = read
		}
		if problem != "" {
			notes = append(notes, fmt.Sprintf("kept acknowledgement %s not confirmed: %s", r[0], problem))
		}
	}
	return notes
}
