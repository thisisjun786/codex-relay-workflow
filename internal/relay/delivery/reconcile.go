package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Affirmative evidence (reconcile.Evidence): the closed list; elapsed time is not on it.
const (
	TurnFound                  = "turn_found"
	ReceiptTurnID              = "receipt_turn_id"
	ConfirmedPreSendRejection  = "confirmed_pre_send_rejection"
	NoEvidence                 = "none"
	reconcileScanLimit         = 200
	reconcileAction            = "daemon_reconciles_delivery"
	correctionUnconfirmed      = "daemon_confirms_correction"
	correctionHeld             = "parent_recovers_held_correction"
	correctionAnswered         = "parent_reads_child_disposition"
	unknownSendHeldAction      = "parent_recovers_unknown_send_lost"
	unknownSendUndecidedAction = "parent_recovers_unknown_send_undecided"
	parentRecoveryThen         = "read the report, then open a fresh execution generation (generation-open) if the work still needs verifying"
)

// RelayProgram is the absolute command a rendered recovery line runs (relay_program): the crw
// binary under its compatibility name.
var RelayProgram = func() []string {
	exe, err := os.Executable()
	if err != nil {
		return []string{"codex-session-relay"}
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	return []string{filepath.Join(filepath.Dir(exe), "codex-session-relay")}
}

func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	safe := true
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("@%+=:,./-_", r)) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

func recoveryCommand(stateDirectory, eventID string) string {
	argv := append(RelayProgram(), "--state", stateDirectory, "show", "--event", eventID)
	for i, a := range argv {
		argv[i] = shellQuote(a)
	}
	return strings.Join(argv, " ")
}

type attemptLost struct{}

func (attemptLost) Error() string { return "attempt lost" }

type attemptChanged struct{ moved bool }

func (a attemptChanged) Error() string { return "attempt changed" }

// Reconciler is reconcile.Reconciler: finding out what actually happened to an uncertain send.
type Reconciler struct {
	Store    *store.Store
	Delivery *Service
	Clock    Clock
}

func NewReconciler(d *Service) *Reconciler {
	return &Reconciler{Store: d.Store, Delivery: d, Clock: d.Clock}
}

func (rc *Reconciler) policy() RetryPolicy { return rc.Delivery.Policy }

const unresolvedWhere = " WHERE (a.internal_state = 'in_flight' OR (a.state = ? AND d.state IN (?, ?)))"

// OpenAttempts is open_attempts with no bound: everything a restart has to look at.
func (rc *Reconciler) OpenAttempts(ctx context.Context) ([]Row, error) {
	return all(ctx, rc.Store, "SELECT a.*, r.parent_task_id AS parent_task_id FROM attempts a JOIN deliveries d ON d.event_id = a.event_id JOIN relationships r ON r.relationship_id = d.relationship_id"+unresolvedWhere+" ORDER BY a.observed_at", HeldUncertain, HeldUncertain, Sending)
}

// ReconcileAttempt is reconcile_attempt.
func (rc *Reconciler) ReconcileAttempt(ctx context.Context, requestID string, adapter Adapter, now *float64) (Obj, error) {
	out, err := rc.reconcile(ctx, requestID, adapter, now)
	var changed attemptChanged
	switch {
	case errors.As(err, &attemptLost{}):
		return rc.recordedLoss(ctx, requestID, nil)
	case errors.As(err, &changed):
		return rc.asItStands(ctx, requestID, changed, nil)
	}
	return out, err
}

func (rc *Reconciler) asItStands(ctx context.Context, requestID string, changed attemptChanged, reading Reading) (Obj, error) {
	attempt, err := one(ctx, rc.Store, "SELECT * FROM attempts WHERE request_id = ?", requestID)
	if err != nil {
		return nil, err
	}
	delivery, err := rc.Delivery.Get(ctx, attempt.S("event_id"))
	if err != nil {
		return nil, err
	}
	evidence := attempt.S("affirmative_evidence")
	if evidence == "" {
		evidence = NoEvidence
	}
	var record any
	if !attempt.N("record") {
		record = loadsObj(attempt.S("record"))
	}
	out := Obj{{Key: "evidence", Value: evidence}, {Key: "state", Value: attempt.Opt("state")}, {Key: "deliveryState", Value: delivery.S("state")}, {Key: "record", Value: record}}
	if changed.moved {
		out = append(out, F{Key: "changed", Value: true}, F{Key: "detail", Value: "another reader settled this attempt while this one read it; nothing was written, and the attempt as it now stands is reported"})
	} else {
		out = append(out, F{Key: "kept", Value: true}, F{Key: "detail", Value: fmt.Sprintf("this attempt is already settled on %s; a reading that found no evidence does not write over it", pyStr(attempt.Opt("affirmative_evidence")))})
	}
	if reading != nil {
		out = append(out, F{Key: "recipientTurn", Value: reading})
	}
	if attempt.S("state") == HeldUncertain && delivery.S("state") == HeldUncertain && delivery.I("attempt_count") == attempt.I("attempt_no") && evidence == NoEvidence {
		aw, err := rc.awaiting(ctx, delivery.S("kind"), out, nil, Row{"hold_reason": delivery.Opt("hold_reason"), "recipient_scan": attempt.Opt("recipient_scan")}, attempt.S("event_id"))
		if err != nil {
			return nil, err
		}
		for _, f := range aw {
			out = set(out, f.Key, f.Value)
		}
	}
	return out, nil
}

func (rc *Reconciler) recordedLoss(ctx context.Context, requestID string, reading Reading) (Obj, error) {
	attempt, err := one(ctx, rc.Store, "SELECT * FROM attempts WHERE request_id = ?", requestID)
	if err != nil {
		return nil, err
	}
	rows, err := all(ctx, rc.Store, "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq DESC", HostLostTurn, attempt.S("event_id"))
	if err != nil {
		return nil, err
	}
	var redelivery any
	for _, row := range rows {
		detail := loadsObj(row.S("detail"))
		if str(detail, "requestId") == requestID {
			redelivery, _ = get(detail, "redelivery")
			break
		}
	}
	evidence := attempt.S("affirmative_evidence")
	if evidence == "" {
		evidence = ReceiptTurnID
	}
	out := Obj{{Key: "evidence", Value: evidence}, {Key: "state", Value: HostLostTurn}, {Key: "redelivery", Value: redelivery}, {Key: "record", Value: loadsObj(attempt.S("record"))}, {Key: "detail", Value: "the host lost this attempt's turn and that is already recorded; nothing further was written"}}
	if reading != nil {
		out = append(out, F{Key: "recipientTurn", Value: reading})
	}
	return out, nil
}

func (rc *Reconciler) reconcile(ctx context.Context, requestID string, adapter Adapter, nowp *float64) (Obj, error) {
	now := rc.Clock.Now()
	if nowp != nil {
		now = *nowp
	}
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
	observation := "missing"
	var facts *Facts
	receiptRead := false
	receipt, err := adapter.GetOperation(requestID)
	if err != nil {
		observation = "unreadable: " + errorLabel(err)
	} else {
		receiptRead = true
	}
	if receipt != nil {
		f := Classify(receipt)
		facts = &f
		observation = f.ReceiptStatus + ":" + f.DeliveryState
		if f.DeliveryState == Dispatched {
			notes, _ := get(receipt, "settingsNotes")
			return rc.settleDispatched(ctx, attempt, delivery, f, observation, adapter, now, notes)
		}
		if f.RetrySafe {
			findings, _ := get(receipt, "settingsFindings")
			return rc.settleFromReceipt(ctx, attempt, delivery, f, ConfirmedPreSendRejection, observation, now, false, nil, findings)
		}
		if f.ReceiptStatus != Unfinished {
			observation += " (not affirmative)"
		}
	}
	if attempt.S("state") == HeldUncertain && attempt.S("affirmative_evidence") == TurnFound {
		return nil, fmt.Errorf("delivery: re-reading a token-confirmed send's turn is the host-loss port's (part B)")
	}
	scanDetail := "not scanned"
	scanned := false
	scan, err := adapter.FindToken(delivery.S("recipient_thread_id"), requestID, reconcileScanLimit, true)
	if err != nil {
		scanDetail = "unreadable: " + errorLabel(err)
	} else {
		scanDetail = fmt.Sprintf("found=%s exhausted=%s scanned=%d", pyStr(scan.Found), pyStr(scan.Exhausted), scan.Scanned)
		if scan.Found {
			return rc.settleFromScan(ctx, attempt, delivery, scan, observation, scanDetail, now)
		}
		scanned = true
	}
	var reading Reading
	answer := ""
	if receiptRead {
		answer = receiptAnswer(facts)
	}
	if scanned && answer != "" {
		reading = ReadUnknownSend(adapter, rc.Clock, attempt, delivery, answer)
		if str(reading, "finding") == Present {
			turn, _ := get(reading, "turnId")
			out, err := rc.settleFromScan(ctx, attempt, delivery, TokenScan{Found: true, TurnID: turn}, observation, "found since the send in turn "+pyStr(turn), now)
			if err != nil {
				return nil, err
			}
			return append(out, F{Key: "recipientTrace", Value: reading}), nil
		}
	}
	out, err := rc.stayHeld(ctx, attempt, delivery, observation, scanDetail, reading)
	if err != nil {
		return nil, err
	}
	if reading != nil {
		out = append(out, F{Key: "recipientTrace", Value: reading})
	}
	stored, err := one(ctx, rc.Store, "SELECT d.hold_reason, a.recipient_scan FROM attempts a JOIN deliveries d ON d.event_id = a.event_id WHERE a.request_id = ?", requestID)
	if err != nil {
		return nil, err
	}
	aw, err := rc.awaiting(ctx, delivery.S("kind"), out, reading, stored, attempt.S("event_id"))
	if err != nil {
		return nil, err
	}
	for _, f := range aw {
		out = set(out, f.Key, f.Value)
	}
	return out, nil
}

func receiptAnswer(facts *Facts) string {
	if facts == nil {
		return MissingReceipt
	}
	if facts.DeliveryState != HeldUncertain || facts.TurnID != nil || facts.RetrySafe {
		return ""
	}
	if facts.ReceiptStatus == Unfinished {
		return UnsettledReceipt
	}
	return SettledReceipt
}

func (rc *Reconciler) settleDispatched(ctx context.Context, attempt, delivery Row, facts Facts, observation string, adapter Adapter, now float64, notes any) (Obj, error) {
	reading := ReadRecipientTurn(adapter, rc.Clock, attempt, delivery, facts.TurnID)
	out, err := rc.settleFromReceipt(ctx, attempt, delivery, facts, ReceiptTurnID, observation, now, str(reading, "finding") != Unknown, notes, nil)
	var changed attemptChanged
	switch {
	case errors.As(err, &attemptLost{}):
		return rc.recordedLoss(ctx, attempt.S("request_id"), reading)
	case errors.As(err, &changed):
		return rc.asItStands(ctx, attempt.S("request_id"), changed, reading)
	case err != nil:
		return nil, err
	}
	out = append(out, F{Key: "recipientTurn", Value: reading})
	if str(reading, "finding") == HostLostTurn {
		if delivery.S("kind") == Completion {
			loss, err := SettleLoss(ctx, rc.Store, rc.Clock, attempt.S("request_id"), reading, observation)
			if err != nil {
				return nil, err
			}
			for _, f := range loss {
				out = set(out, f.Key, f.Value)
			}
		} else {
			out = set(out, "redelivery", ReportOnly)
		}
	} else if delivery.S("kind") == Completion {
		if _, err := RecordUndecided(ctx, rc.Store, attempt.S("request_id"), reading); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// forceCurrent stands in for Python's mock.patch of _is_current (a test seam, never set in
// production): the guarded UPDATE, not the snapshot, must decide.
var forceCurrent bool

func (rc *Reconciler) isCurrent(attempt, delivery Row) bool {
	if forceCurrent {
		return true
	}
	if attempt.I("attempt_no") != delivery.I("attempt_count") {
		return false
	}
	s := delivery.S("state")
	return s != Dispatched && s != Acknowledged && s != Superseded
}

func (rc *Reconciler) settleFromReceipt(ctx context.Context, attempt, delivery Row, facts Facts, evidence, observation string, now float64, turnsChecked bool, notes, findings any) (Obj, error) {
	record, err := AttemptRecord(facts, attempt.S("request_id"), attempt.S("event_id"), attempt.I("attempt_no"), delivery.S("recipient_task_id"), "unknown", rc.Clock.ISO(),
		Obj{{Key: "operationReceiptChecked", Value: true}, {Key: "recipientTurnsChecked", Value: turnsChecked}, {Key: "affirmativeEvidence", Value: evidence}, {Key: "checkedAt", Value: rc.Clock.ISO()}})
	if err != nil {
		return nil, err
	}
	var next, hold any
	if facts.RetrySafe {
		reason := "presend"
		if facts.DeliveryState == DeferredBusy {
			reason = "busy"
		}
		next = now + rc.policy().DelayFor(attempt.I("attempt_no")+1, reason)
		if attempt.I("attempt_no") >= rc.policy().CapFor(reason) {
			hold, next = rc.policy().CapReason(reason), nil
		}
	}
	var dispatchEvidence any
	if facts.DeliveryState == Dispatched {
		dispatchEvidence = "transport_accepted"
	}
	anchor, err := rc.write(ctx, attempt, delivery, record, facts.DeliveryState, evidence, observation, "not scanned", next, writeOpts{hold: hold, dispatchEvidence: dispatchEvidence, dispatchTurn: facts.TurnID, settingsRefusal: settingsRefusalOf(facts, findings), notes: notes})
	if err != nil {
		return nil, err
	}
	return withAnchor(Obj{{Key: "evidence", Value: evidence}, {Key: "state", Value: facts.DeliveryState}, {Key: "record", Value: record}}, anchor), nil
}

func withAnchor(out Obj, anchor any) Obj {
	if anchor != nil {
		out = append(out, F{Key: "anchor", Value: anchor})
	}
	return out
}

func unfinishedRecord(attempt, delivery Row, observedAt string) Obj {
	return Obj{{Key: "requestId", Value: attempt.S("request_id")}, {Key: "eventId", Value: attempt.S("event_id")}, {Key: "attemptNo", Value: attempt.I("attempt_no")}, {Key: "recipientTaskId", Value: delivery.S("recipient_task_id")},
		{Key: "deliveryState", Value: HeldUncertain}, {Key: "sendAttempted", Value: "unknown"}, {Key: "retrySafe", Value: false}, {Key: "transportReceiptStatus", Value: Unfinished}, {Key: "failedOperation", Value: nil}, {Key: "turnId", Value: nil},
		{Key: "recipientStatusBefore", Value: "unknown"}, {Key: "recipientApprovalPolicy", Value: nil}, {Key: "observedAt", Value: observedAt}}
}

func (rc *Reconciler) settleFromScan(ctx context.Context, attempt, delivery Row, scan TokenScan, observation, scanDetail string, now float64) (Obj, error) {
	var record Obj
	if attempt.N("record") {
		record = unfinishedRecord(attempt, delivery, rc.Clock.ISO())
	} else {
		record = loadsObj(attempt.S("record"))
	}
	record = set(record, "reconciliation", Obj{{Key: "operationReceiptChecked", Value: true}, {Key: "recipientTurnsChecked", Value: true}, {Key: "affirmativeEvidence", Value: TurnFound}, {Key: "checkedAt", Value: rc.Clock.ISO()}})
	anchor, err := rc.write(ctx, attempt, delivery, record, str(record, "deliveryState"), TurnFound, observation, scanDetail, nil, writeOpts{aggregate: Dispatched, dispatchEvidence: "turn_found", dispatchTurn: scan.TurnID})
	if err != nil {
		return nil, err
	}
	return withAnchor(Obj{{Key: "evidence", Value: TurnFound}, {Key: "state", Value: Dispatched}, {Key: "record", Value: record}}, anchor), nil
}

func (rc *Reconciler) stayHeld(ctx context.Context, attempt, delivery Row, observation, scanDetail string, reading Reading) (Obj, error) {
	var record Obj
	if attempt.N("record") {
		record = unfinishedRecord(attempt, delivery, rc.Clock.ISO())
	} else {
		record = loadsObj(attempt.S("record"))
	}
	record = set(record, "reconciliation", Obj{{Key: "operationReceiptChecked", Value: true}, {Key: "recipientTurnsChecked", Value: !strings.Contains(scanDetail, "not scanned")}, {Key: "affirmativeEvidence", Value: NoEvidence}, {Key: "checkedAt", Value: rc.Clock.ISO()}})
	mark := scanDetail
	var hold any
	if reading != nil {
		if u, _ := get(reading, "undecided"); truthy(u) {
			mark, hold = unknownUndecided+pyStr(u), UnknownSendUndecided
		} else if str(reading, "finding") == UnknownSendLost {
			mark, hold = unknownLostMark, UnknownSendLost
		}
	}
	if _, err := rc.write(ctx, attempt, delivery, record, HeldUncertain, NoEvidence, observation, mark, nil, writeOpts{hold: hold, keepUnknown: hold == nil, clearDispatch: true, expectScan: hold != nil}); err != nil {
		return nil, err
	}
	return Obj{{Key: "evidence", Value: NoEvidence}, {Key: "state", Value: HeldUncertain}, {Key: "missing", Value: "no turn id in the operation receipt, no matching turn in the recipient's items, and no confirmed pre-send rejection"},
		{Key: "operationObservation", Value: observation}, {Key: "recipientScan", Value: scanDetail}, {Key: "record", Value: record}}, nil
}

type writeOpts struct {
	aggregate                              string
	dispatchEvidence, dispatchTurn, hold   any
	keepUnknown, clearDispatch, expectScan bool
	settingsRefusal, notes                 any
}

func boolFlag(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func (rc *Reconciler) write(ctx context.Context, attempt, delivery Row, record Obj, state, evidence, observation, scanDetail string, next any, o writeOpts) (any, error) {
	now := rc.Clock.ISO()
	current := rc.isCurrent(attempt, delivery)
	var anchor any
	aggregate := o.aggregate
	if aggregate == "" {
		aggregate = state
	}
	err := rc.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		updated, err := execSQL(ctx, rc.Store, "UPDATE attempts SET internal_state = 'settled', state = ?, record = ?, operation_observation = ?, recipient_scan = CASE WHEN (? = ? AND recipient_scan LIKE ?) OR (? AND ? = ? AND recipient_scan LIKE ?) THEN recipient_scan ELSE ? END, affirmative_evidence = ?, reconciled_at = ? WHERE request_id = ? AND (state IS NULL OR state <> ?) AND internal_state IS ? AND state IS ? AND affirmative_evidence IS ? AND NOT (? = ? AND affirmative_evidence IS NOT NULL AND affirmative_evidence <> ?) AND (? = 0 OR recipient_scan IS ?)",
			state, dumps(record), observation, state, Dispatched, TurnCheckUndecided+":%", boolFlag(o.keepUnknown), state, HeldUncertain, unknownMarkPrefix+"%", scanDetail,
			evidence, now, attempt.S("request_id"), HostLostTurn, attempt.Opt("internal_state"), attempt.Opt("state"), attempt.Opt("affirmative_evidence"),
			evidence, NoEvidence, NoEvidence, boolFlag(o.expectScan), attempt.Opt("recipient_scan"))
		if err != nil {
			return err
		}
		if updated != 1 {
			now, err := one(ctx, rc.Store, "SELECT internal_state, state, affirmative_evidence, recipient_scan FROM attempts WHERE request_id = ?", attempt.S("request_id"))
			if err != nil {
				return err
			}
			if now != nil && now.S("state") == HostLostTurn {
				return attemptLost{}
			}
			moved := now == nil || now.Opt("internal_state") != attempt.Opt("internal_state") || now.Opt("state") != attempt.Opt("state") || now.Opt("affirmative_evidence") != attempt.Opt("affirmative_evidence") || (o.expectScan && now.Opt("recipient_scan") != attempt.Opt("recipient_scan"))
			return attemptChanged{moved: moved}
		}
		if current {
			var before Row
			if o.hold != nil {
				if before, err = one(ctx, rc.Store, "SELECT hold_reason FROM deliveries WHERE event_id = ?", attempt.S("event_id")); err != nil {
					return err
				}
			}
			dispatchEvidence := o.dispatchEvidence
			if dispatchEvidence == nil {
				dispatchEvidence = delivery.Opt("dispatch_evidence")
			}
			promoted, err := execSQL(ctx, rc.Store, "UPDATE deliveries SET state = ?, next_eligible_at = ?, hold_reason = CASE WHEN ? AND hold_reason IN (?, ?) THEN hold_reason ELSE ? END, dispatch_evidence = CASE WHEN ? THEN NULL ELSE ? END, dispatch_turn_id = CASE WHEN ? THEN NULL ELSE COALESCE(?, dispatch_turn_id) END, lease_owner = NULL, lease_until = NULL, updated_at = ? WHERE event_id = ? AND attempt_count = ? AND state NOT IN (?, 'acknowledged', 'superseded')",
				aggregate, next, boolFlag(o.keepUnknown), UnknownSendLost, UnknownSendUndecided, o.hold, boolFlag(o.clearDispatch), dispatchEvidence, boolFlag(o.clearDispatch), o.dispatchTurn, now, attempt.S("event_id"), attempt.I("attempt_no"), Dispatched)
			if err != nil {
				return err
			}
			if promoted == 1 && aggregate == Dispatched {
				if anchor, err = rc.bindPromotedAnchor(ctx, attempt, delivery, o.dispatchTurn); err != nil {
					return err
				}
			}
			var previous any
			if before != nil {
				previous = before.Opt("hold_reason")
			}
			if promoted == 1 && o.hold != nil && previous != o.hold {
				if err := journal(ctx, rc.Store, UnknownSendHoldNamed, attempt.S("request_id"), Obj{{Key: "hold", Value: o.hold}, {Key: "previous", Value: previous}, {Key: "eventId", Value: attempt.S("event_id")}}, now); err != nil {
					return err
				}
			}
		}
		if err := journal(ctx, rc.Store, "reconciled", attempt.S("request_id"), Obj{{Key: "evidence", Value: evidence}, {Key: "state", Value: aggregate}, {Key: "settingsRefusal", Value: o.settingsRefusal}}, now); err != nil {
			return err
		}
		if list, _ := o.notes.([]any); len(list) > 0 {
			noted, err := one(ctx, rc.Store, "SELECT 1 FROM journal WHERE subject = ? AND +kind = ? AND (CASE WHEN json_valid(detail) THEN json_extract(detail, '$.requestId') END) = ?", attempt.S("event_id"), SettingsNoted, attempt.S("request_id"))
			if err != nil {
				return err
			}
			if noted == nil {
				return journal(ctx, rc.Store, SettingsNoted, attempt.S("event_id"), Obj{{Key: "requestId", Value: attempt.S("request_id")}, {Key: "notes", Value: o.notes}}, now)
			}
		}
		return nil
	})
	return anchor, err
}

func (rc *Reconciler) bindPromotedAnchor(ctx context.Context, attempt, delivery Row, dispatchTurn any) (any, error) {
	if delivery.S("kind") != Revision {
		return nil, nil
	}
	turn, _ := dispatchTurn.(string)
	if turn == "" {
		turn = delivery.S("dispatch_turn_id")
	}
	if turn == "" {
		return nil, nil
	}
	event, err := one(ctx, rc.Store, "SELECT relationship_id, execution_generation FROM events WHERE event_id = ?", attempt.S("event_id"))
	if err != nil || event == nil {
		return nil, err
	}
	return BindAnchorIn(ctx, rc.Store, rc.Clock, event.S("relationship_id"), event.I("execution_generation"), turn)
}

// BindAnchorIn is registry.bind_anchor_in, inside the caller's transaction: bound, unchanged,
// conflict (journalled, never overwritten) or ineligible.
func BindAnchorIn(ctx context.Context, s *store.Store, clock Clock, rid string, number int64, turn string) (string, error) {
	if strings.TrimSpace(turn) == "" {
		return "ineligible", nil
	}
	current, err := one(ctx, s, "SELECT anchor_state, dispatch_turn_id FROM generations WHERE relationship_id = ? AND execution_generation = ?", rid, number)
	if err != nil || current == nil {
		return "ineligible", err
	}
	now := clock.ISO()
	if current.S("anchor_state") == "bound" {
		if current.S("dispatch_turn_id") == turn {
			return "unchanged", nil
		}
		return "conflict", journal(ctx, s, "anchor_conflict", rid, Obj{{Key: "generation", Value: number}, {Key: "boundTo", Value: current.Opt("dispatch_turn_id")}, {Key: "offered", Value: turn}}, now)
	}
	if _, err := execSQL(ctx, s, "UPDATE generations SET anchor_state = ?, dispatch_turn_id = ?, bound_at = ? WHERE relationship_id = ? AND execution_generation = ?", "bound", turn, now, rid, number); err != nil {
		return "", err
	}
	return "bound", journal(ctx, s, "anchor_bound", rid, Obj{{Key: "generation", Value: number}}, now)
}

// awaiting is reconcile._awaiting for the kinds this package delivers.
func (rc *Reconciler) awaiting(ctx context.Context, kind string, outcome Obj, reading Reading, stored Row, eventID string) (Obj, error) {
	if str(outcome, "state") != HeldUncertain {
		return nil, nil
	}
	correction := kind == Revision
	note, err := one(ctx, rc.Store, "SELECT reason FROM delivery_supersession WHERE event_id = ?", eventID)
	if err != nil {
		return nil, err
	}
	superseded := ""
	if note != nil {
		superseded = note.S("reason")
	} else if superseded, err = rc.Delivery.SupersessionReason(ctx, eventID); err != nil {
		return nil, err
	}
	if superseded != "" {
		action := "none"
		if correction && superseded == SupersededRevision {
			action = correctionAnswered
		}
		return Obj{{Key: "nextExpectedAction", Value: action}, {Key: "reason", Value: "superseded:" + superseded}}, nil
	}
	var hold, mark any
	if stored != nil {
		hold, mark = stored.Opt("hold_reason"), stored.Opt("recipient_scan")
	}
	var action string
	var reason any
	switch hold {
	case UnknownSendLost:
		action, reason = unknownSendHeldAction, UnknownSendLost
	case UnknownSendUndecided:
		action, reason = unknownSendUndecidedAction, mark
		if !truthy(mark) {
			reason = UnknownSendUndecided
		}
	default:
		action = reconcileAction
		if correction {
			action = correctionUnconfirmed
		}
		reason, _ = get(outcome, "missing")
		if reading != nil {
			if d, _ := get(reading, "detail"); truthy(d) {
				reason = d
			}
		}
		return Obj{{Key: "nextExpectedAction", Value: action}, {Key: "reason", Value: reason}}, nil
	}
	if correction {
		action = correctionHeld
	}
	directory := filepath.Dir(rc.Store.Path)
	if abs, err := filepath.Abs(directory); err == nil {
		directory = abs
	}
	return Obj{{Key: "nextExpectedAction", Value: action}, {Key: "reason", Value: reason},
		{Key: "recovery", Value: Obj{{Key: "actor", Value: "parent"}, {Key: "reason", Value: reason}, {Key: "command", Value: recoveryCommand(directory, eventID)}, {Key: "then", Value: parentRecoveryThen}}}}, nil
}

// RecoverOnStart is recover_on_start: establish what happened; send nothing.
func (rc *Reconciler) RecoverOnStart(ctx context.Context, adapter Adapter, now *float64) (Obj, error) {
	attempts, err := rc.OpenAttempts(ctx)
	if err != nil {
		return nil, err
	}
	reconciled, held, dispatched := []any{}, []any{}, []any{}
	for _, a := range attempts {
		outcome, err := rc.ReconcileAttempt(ctx, a.S("request_id"), adapter, now)
		if err != nil {
			return nil, err
		}
		entry := Obj{{Key: "requestId", Value: a.S("request_id")}}
		for _, f := range outcome {
			if f.Key != "record" {
				entry = append(entry, f)
			}
		}
		reconciled = append(reconciled, entry)
		if str(outcome, "state") == HeldUncertain {
			held = append(held, a.S("request_id"))
		}
	}
	rows, err := all(ctx, rc.Store, "SELECT d.event_id FROM deliveries d LEFT JOIN acks a ON a.event_id = d.event_id WHERE d.state = ? AND d.kind = ? AND a.event_id IS NULL", Dispatched, Completion)
	if err != nil {
		return nil, err
	}
	for _, r := range rows {
		dispatched = append(dispatched, r.S("event_id"))
	}
	return Obj{{Key: "reconciled", Value: reconciled}, {Key: "heldUncertain", Value: held}, {Key: "awaitingAck", Value: dispatched}, {Key: "resent", Value: []any{}}}, nil
}
