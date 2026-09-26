package delivery

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Host-loss words (hostloss.py).
const (
	Present           = "present"
	Unknown           = "unknown"
	tokenScanLimit    = 200
	Requeued          = "queued"
	HeldRedelivery    = "held"
	NotMoved          = "not_moved"
	ReportOnly        = "report_only"
	ListingBoundedW   = "listing_bounded"
	ListingEmptyW     = "listing_empty"
	TokenScanBounded  = "token_scan_bounded"
	TokenWithoutTurn  = "token_without_turn"
	TokenInOtherItem  = "token_in_other_item"
	NoSendTime        = "no_send_time"
	NoTurn            = "no_turn"
	ReceiptUnsettled  = "receipt_unsettled"
	ReceiptMissing    = "receipt_missing"
	SettledReceipt    = "settled"
	UnsettledReceipt  = "unsettled"
	MissingReceipt    = "missing"
	undecidedMark     = TurnCheckUndecided + ":"
	unknownLostMark   = UnknownSendLost + ":no_trace"
	unknownUndecided  = UnknownSendUndecided + ":"
	unknownMarkPrefix = "unknown_send_"
)

var terminalTurn = []string{"completed", "interrupted", "failed"}
var deliveredAttemptStates = []string{Dispatched, HeldUncertain}

func epoch(stamp string) (float64, bool) {
	t, err := time.Parse("2006-01-02T15:04:05.999999-07:00", stamp)
	if err != nil {
		return 0, false
	}
	return float64(t.UnixMicro()) / 1e6, true
}

// Reading is what a recipient's own turns and items say about an attempt.
type Reading = Obj

func newReading(turnID any) Reading {
	return Obj{{Key: "turnId", Value: turnID}, {Key: "finding", Value: Unknown}, {Key: "status", Value: nil}, {Key: "detail", Value: nil}, {Key: "undecided", Value: nil}}
}

func allowance() float64 { return TurnStartPrecisionSeconds + DispatchTurnSkewSeconds }

// ReadRecipientTurn is hostloss.read_recipient_turn. Reads only.
func ReadRecipientTurn(adapter Adapter, clock Clock, attempt, delivery Row, turnID any) Reading {
	r := newReading(turnID)
	thread := delivery.S("recipient_thread_id")
	turn, _ := turnID.(string)
	if turn == "" {
		return set(set(r, "detail", "the attempt names no turn"), "undecided", NoTurn)
	}
	sentAt, ok := epoch(attempt.S("sent_at"))
	if !ok {
		return set(set(r, "detail", "the attempt has no send time, so absence cannot be bounded"), "undecided", NoSendTime)
	}
	allow := allowance()
	presence, err := adapter.FindDispatchedTurn(thread, turn, sentAt)
	var bounded *ListingBounded
	var empty *ListingEmpty
	switch {
	case errors.As(err, &bounded):
		return set(set(r, "detail", "undecided: "+bounded.Message), "undecided", ListingBoundedW)
	case errors.As(err, &empty):
		if clock.Now() < sentAt+allow {
			return set(r, "detail", fmt.Sprintf("the recipient lists no turns yet, and the send is less than %.0f s old: %s", allow, empty.Message))
		}
		return set(set(r, "detail", "undecided: "+empty.Message), "undecided", ListingEmptyW)
	case err != nil:
		return set(r, "detail", "unreadable: "+errorLabel(err))
	}
	listed := presence.Finding == TurnPresent
	where := "does not list this turn"
	if listed {
		status := presence.Turn.Status
		if !slices.Contains(terminalTurn, status) {
			return set(set(set(r, "finding", Present), "status", status), "detail", fmt.Sprintf("the recipient lists this turn (%s)", status))
		}
		own, err := adapter.FindTokenInTurn(thread, attempt.S("request_id"), turn, InTurnItemsMax)
		if err != nil {
			return set(r, "detail", fmt.Sprintf("unreadable: the recipient lists this turn (%s), but its items could not be read for this attempt's message: %s", status, errorLabel(err)))
		}
		if own.Found {
			return set(set(set(r, "finding", Present), "status", status), "detail", fmt.Sprintf("the recipient lists this turn (%s) with this attempt's message in it", status))
		}
		where = fmt.Sprintf("lists this turn %s without this attempt's message among its first %d items", status, own.Scanned)
	}
	if clock.Now() < sentAt+allow {
		if listed {
			return set(r, "detail", fmt.Sprintf("the recipient %s, but the send is less than %.0f s old, too recent to call the turn lost", where, allow))
		}
		return set(r, "detail", fmt.Sprintf("the recipient does not list this turn yet (%s), but the send is less than %.0f s old, too recent to call the turn lost", presence.Stop, allow))
	}
	scan, err := adapter.FindTokenSince(thread, attempt.S("request_id"), presence.Older, tokenScanLimit)
	if err != nil {
		return set(r, "detail", fmt.Sprintf("the recipient %s, and its items could not be read for this attempt's token: %s", where, errorLabel(err)))
	}
	if scan.Found {
		if listed {
			return set(set(r, "finding", Present), "detail", fmt.Sprintf("the recipient %s, but this attempt's message is in its items (turn %s)", where, pyStr(scan.TurnID)))
		}
		return set(set(set(r, "finding", Present), "detail", fmt.Sprintf("the recipient does not list this turn, but this attempt's token is in its items (turn %s)", pyStr(scan.TurnID))), "undecided", TokenWithoutTurn)
	}
	if scan.OtherKind != nil {
		return set(set(r, "detail", fmt.Sprintf("undecided: the recipient %s, and this attempt's token is in its items only in an item of type %s (turn %s), which is neither the delivered message nor agent output; not sent again", where, pyStr(scan.OtherKind), pyStr(scan.OtherTurn))), "undecided", TokenInOtherItem)
	}
	if !scan.Exhausted {
		return set(set(r, "detail", fmt.Sprintf("undecided: the recipient %s, and %d items did not reach history older than the send, so the token's absence is not shown", where, scan.Scanned)), "undecided", TokenScanBounded)
	}
	if listed {
		return set(set(r, "finding", HostLostTurn), "detail", fmt.Sprintf("the recipient %s, and this attempt's token is not among the %d items since the send (%d listed turns begun since it): the host lost the turn's content", where, scan.Scanned, len(presence.Seen)))
	}
	return set(set(r, "finding", HostLostTurn), "detail", fmt.Sprintf("the recipient's turn list has no such turn (%s after %d turns) and this attempt's token is not among the %d items since the send (%d listed turns begun since it)", presence.Stop, presence.Scanned, scan.Scanned, len(presence.Seen)))
}

// RecordUndecided is hostloss.record_undecided; returns the rows changed.
func RecordUndecided(ctx context.Context, s *store.Store, requestID string, reading Reading) (int64, error) {
	var wanted any
	if u, _ := get(reading, "undecided"); truthy(u) {
		wanted = undecidedMark + pyStr(u)
	} else if str(reading, "finding") != Present {
		return 0, nil
	}
	var changed int64
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		row, err := one(ctx, s, "SELECT internal_state, state, recipient_scan FROM attempts WHERE request_id = ?", requestID)
		if err != nil || row == nil || row.S("internal_state") != "settled" || !slices.Contains(deliveredAttemptStates, row.S("state")) {
			return err
		}
		current := row.S("recipient_scan")
		if wanted == nil && !strings.HasPrefix(current, undecidedMark) {
			return nil
		}
		if wanted != nil && !row.N("recipient_scan") && current == wanted {
			return nil
		}
		changed, err = execSQL(ctx, s, "UPDATE attempts SET recipient_scan = ? WHERE request_id = ?", wanted, requestID)
		return err
	})
	return changed, err
}

var errRaced = errors.New("raced")

// SettleLoss is hostloss.settle: record the loss and put the obligation back, once.
func SettleLoss(ctx context.Context, s *store.Store, clock Clock, requestID string, reading Reading, observation any) (Obj, error) {
	now := clock.ISO()
	var record Obj
	var hold any
	notMoved := false
	err := s.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		attempt, err := one(ctx, s, "SELECT * FROM attempts WHERE request_id = ?", requestID)
		if err != nil {
			return err
		}
		earlier, err := one(ctx, s, "SELECT COUNT(*) AS c FROM attempts WHERE event_id = ? AND state = ? AND request_id <> ?", attempt.S("event_id"), HostLostTurn, requestID)
		if err != nil {
			return err
		}
		if earlier.I("c") > 0 {
			hold = HostLostTurn
		}
		moved, err := execSQL(ctx, s, "UPDATE deliveries SET state = ?, hold_reason = ?, next_eligible_at = NULL, dispatch_evidence = ?, lease_owner = NULL, lease_until = NULL, updated_at = ? WHERE event_id = ? AND kind = ? AND state = ? AND attempt_count = ? AND hold_reason IS NULL AND NOT EXISTS (SELECT 1 FROM acks k WHERE k.event_id = deliveries.event_id AND (k.verified = 'verified' OR ? IS NULL OR k.ack_at >= ?))",
			Queued, hold, HostLostTurn, now, attempt.S("event_id"), Completion, Dispatched, attempt.I("attempt_no"), attempt.Opt("sent_at"), attempt.Opt("sent_at"))
		if err != nil {
			return err
		}
		if moved != 1 {
			notMoved = true
			return nil
		}
		record = loadsObj(attempt.S("record"))
		evidence := attempt.S("affirmative_evidence")
		if evidence == "" {
			evidence = "receipt_turn_id"
		}
		record = set(record, "reconciliation", Obj{{Key: "operationReceiptChecked", Value: observation != nil}, {Key: "recipientTurnsChecked", Value: true}, {Key: "affirmativeEvidence", Value: evidence}, {Key: "checkedAt", Value: now}})
		if err := AssertAttemptInvariants(record); err != nil {
			return err
		}
		marked, err := execSQL(ctx, s, "UPDATE attempts SET state = ?, record = ?, operation_observation = COALESCE(?, operation_observation), recipient_scan = ?, affirmative_evidence = ?, reconciled_at = ? WHERE request_id = ? AND internal_state = 'settled' AND state IN (?, ?)",
			HostLostTurn, dumps(record), observation, str(reading, "detail"), evidence, now, requestID, Dispatched, HeldUncertain)
		if err != nil {
			return err
		}
		if marked != 1 {
			return errRaced
		}
		redelivery := Requeued
		if hold != nil {
			redelivery = HeldRedelivery
		}
		turnID, _ := get(reading, "turnId")
		return journal(ctx, s, HostLostTurn, attempt.S("event_id"), Obj{{Key: "requestId", Value: requestID}, {Key: "turnId", Value: turnID}, {Key: "redelivery", Value: redelivery}, {Key: "detail", Value: str(reading, "detail")}}, now)
	})
	if errors.Is(err, errRaced) {
		return Obj{{Key: "redelivery", Value: NotMoved}, {Key: "redeliveryDetail", Value: "the attempt changed while the loss was being recorded"}}, nil
	}
	if err != nil {
		return nil, err
	}
	if notMoved {
		return Obj{{Key: "redelivery", Value: NotMoved}, {Key: "redeliveryDetail", Value: "the delivery is no longer this attempt's unheld, unacknowledged dispatch"}}, nil
	}
	redelivery := Requeued
	if hold != nil {
		redelivery = HeldRedelivery
	}
	return Obj{{Key: "state", Value: HostLostTurn}, {Key: "redelivery", Value: redelivery}, {Key: "holdReason", Value: hold}, {Key: "record", Value: record}}, nil
}

func foldCandidates(p TurnPresence, sentAt float64) []TurnInfo {
	var out []TurnInfo
	for _, t := range p.SeenTurns {
		if t.StartedAt != nil && *t.StartedAt <= sentAt+TurnStartPrecisionSeconds {
			out = append(out, t)
		}
	}
	if p.StopTurn != nil {
		out = append(out, *p.StopTurn)
	}
	return out
}

// ReadUnknownSend is hostloss.read_unknown_send. Reads only; never allows a second send.
func ReadUnknownSend(adapter Adapter, clock Clock, attempt, delivery Row, receipt string) Reading {
	r := Obj{{Key: "turnId", Value: nil}, {Key: "finding", Value: Unknown}, {Key: "detail", Value: nil}, {Key: "undecided", Value: nil}, {Key: "pending", Value: false}}
	thread := delivery.S("recipient_thread_id")
	request := attempt.S("request_id")
	sentAt, ok := epoch(attempt.S("sent_at"))
	if !ok {
		return set(set(r, "detail", "the attempt has no send time, so absence cannot be bounded"), "undecided", NoSendTime)
	}
	allow := allowance()
	pending := func(detail string) Reading { return set(set(r, "detail", detail), "pending", true) }
	undecided := func(detail, why string) Reading { return set(set(r, "detail", detail), "undecided", why) }
	if clock.Now() < sentAt+allow {
		return pending(fmt.Sprintf("the send is less than %.0f s old, too recent to call it lost; read again", allow))
	}
	presence, err := adapter.FindDispatchedTurn(thread, "", sentAt)
	var bounded *ListingBounded
	var empty *ListingEmpty
	switch {
	case errors.As(err, &bounded):
		return undecided("undecided: "+bounded.Message, ListingBoundedW)
	case errors.As(err, &empty):
		return undecided("undecided: "+empty.Message, ListingEmptyW)
	case err != nil:
		return pending("unreadable: " + errorLabel(err))
	}
	folded := foldCandidates(presence, sentAt)
	var running []string
	for _, t := range presence.SeenTurns {
		if !slices.Contains(terminalTurn, t.Status) {
			running = append(running, t.TurnID)
		}
	}
	if presence.StopTurn != nil && !slices.Contains(terminalTurn, presence.StopTurn.Status) {
		running = append(running, presence.StopTurn.TurnID)
	}
	if len(running) > 0 {
		return pending(fmt.Sprintf("a turn begun since the send is still running (%s); read again once it ends", running[0]))
	}
	scan, err := adapter.FindTokenSince(thread, request, presence.Older, tokenScanLimit)
	if err != nil {
		return pending("unreadable: the recipient's items could not be read for this attempt's token: " + errorLabel(err))
	}
	if scan.Found {
		return set(set(set(r, "finding", Present), "turnId", scan.TurnID), "detail", fmt.Sprintf("this attempt's message is in the recipient's items since the send (turn %s, %d items read)", pyStr(scan.TurnID), scan.Scanned))
	}
	type owned struct {
		turn TurnInfo
		scan TokenScan
	}
	var owns []owned
	for _, turn := range folded {
		own, err := adapter.FindTokenInTurn(thread, request, turn.TurnID, InTurnItemsMax)
		if err != nil {
			return pending(fmt.Sprintf("unreadable: the items of turn %s, one the send could have been folded into, could not be read: %s", turn.TurnID, errorLabel(err)))
		}
		if own.Found {
			return set(set(set(r, "finding", Present), "turnId", turn.TurnID), "detail", fmt.Sprintf("this attempt's message is in turn %s, begun by the send, which the send was folded into", turn.TurnID))
		}
		owns = append(owns, owned{turn, own})
	}
	for _, o := range owns {
		if o.scan.OtherKind != nil {
			return undecided(fmt.Sprintf("undecided: this attempt's token is in turn %s, which the send could have been folded into, only in an item of type %s, which is neither the delivered message nor agent output; not sent again", o.turn.TurnID, pyStr(o.scan.OtherKind)), TokenInOtherItem)
		}
	}
	if scan.OtherKind != nil {
		return undecided(fmt.Sprintf("undecided: this attempt's token is in the recipient's items only in an item of type %s (turn %s), which is neither the delivered message nor agent output; not sent again", pyStr(scan.OtherKind), pyStr(scan.OtherTurn)), TokenInOtherItem)
	}
	if !scan.Exhausted {
		return undecided(fmt.Sprintf("undecided: %d items did not reach history older than the send, so the token's absence is not shown", scan.Scanned), TokenScanBounded)
	}
	for _, o := range owns {
		if !o.scan.Exhausted {
			return undecided(fmt.Sprintf("undecided: turn %s, begun by the send, could have taken it, and its first %d items did not reach its end", o.turn.TurnID, o.scan.Scanned), TokenScanBounded)
		}
	}
	if receipt != SettledReceipt {
		said := "the transport has not settled this request's receipt (in_progress_or_unknown)"
		why := ReceiptUnsettled
		if receipt == MissingReceipt {
			said, why = "the transport holds no receipt for this request id", ReceiptMissing
		}
		return undecided("undecided: the recipient keeps no trace of this send, but "+said, why)
	}
	foldedText := ""
	if len(folded) > 0 {
		ids := make([]string, len(folded))
		for i, t := range folded {
			ids[i] = t.TurnID
		}
		foldedText = " nor in " + strings.Join(ids, ", ") + ", the turns begun by it"
	}
	return set(set(r, "finding", UnknownSendLost), "detail", fmt.Sprintf("the recipient keeps no trace of this send: its turn list (%s after %d turns) shows %d turns begun since it, none running, and this attempt's token is not among the %d items since it%s; held for the parent, never sent again", presence.Stop, presence.Scanned, len(presence.Seen), scan.Scanned, foldedText))
}
