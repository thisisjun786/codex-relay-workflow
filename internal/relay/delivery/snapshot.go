package delivery

import (
	"context"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/mergeturn"
)

// reportedState is _reported_state: what an operator should read, beside the raw state.
func reportedState(row Row, ack Row, grant string, superseded Row) string {
	if ack != nil && ack.S("verified") == "verified" && ack.I("accepted") != 0 {
		return "acknowledged"
	}
	if row.S("kind") == MergeTurnGrant {
		switch {
		case grant == mergeturn.GrantAnswered:
			return "grant_acknowledged"
		case grant != "":
			return "superseded:" + grant
		case row.S("state") == Dispatched:
			return "dispatched_awaiting_grant_acknowledgement"
		}
	}
	state := row.S("state")
	hold := row.S("hold_reason")
	switch {
	case state == InboxOnly:
		return "stored_not_woken"
	case state == Dispatched:
		return "dispatched_awaiting_ack"
	case state == HeldUncertain:
		if superseded != nil {
			return "superseded:" + superseded.S("reason")
		}
		if hold != "" {
			return "held:" + hold
		}
		return "held_uncertain_awaiting_evidence"
	case state == Queued && row.S("dispatch_evidence") == HostLostTurn && hold == "":
		return "redelivering:" + HostLostTurn
	case hold != "":
		return "held:" + hold
	}
	return state
}

// phase is _phase for the kinds this package delivers (the settings-hold reading belongs to
// todos 25/22).
func phase(row Row, attempts []Row, ack, failure Row, grant string, superseded Row, pacing Obj) string {
	if ack != nil && ack.S("verified") == "verified" {
		if ack.I("accepted") != 0 {
			return "acknowledged"
		}
		return "rejected"
	}
	if row.S("kind") == MergeTurnGrant && grant != "" {
		if grant == mergeturn.GrantAnswered {
			return "grant_acknowledged"
		}
		return "superseded:" + grant
	}
	state, hold := row.S("state"), row.S("hold_reason")
	switch {
	case state == Superseded:
		return "superseded"
	case superseded != nil:
		return "superseded:" + superseded.S("reason")
	case state == InboxOnly || hold == PushChannelClosed:
		return "channel_closed"
	case state == Dispatched:
		if row.S("kind") == Revision {
			return "awaiting_child_receipt"
		}
		if n := len(attempts); n > 0 && strings.HasPrefix(attempts[n-1].S("recipient_scan"), TurnCheckUndecided+":") {
			return "awaiting_ack:" + TurnCheckUndecided
		}
		return "awaiting_ack"
	case state == DeferredBusy:
		return "parent_busy"
	}
	var latest Row
	for _, a := range attempts {
		if a.S("internal_state") == "settled" {
			latest = a
		}
	}
	record := Obj{}
	if latest != nil && latest.S("record") != "" {
		record = loadsObj(latest.S("record"))
	}
	failed, _ := get(record, "failedOperation")
	failedText, _ := failed.(string)
	switch {
	case state == HeldUncertain:
		if hold != "" {
			return "held:" + hold
		}
		if t, _ := get(record, "turnId"); truthy(t) {
			return "turn_accepted"
		}
		return "outcome_unknown"
	case state == WithheldPreSend && latest != nil:
		if failedText == "thread/resume" && failure != nil && failure.S("operation") == "settings_check" {
			return "settings_rejected"
		}
		if failedText != "" {
			return "withheld:" + failedText
		}
		return "withheld_pre_send"
	case state == WithheldPreSend:
		if failure != nil {
			return "withheld:" + failure.S("operation")
		}
		return "withheld_pre_send"
	case hold != "":
		return "held:" + hold
	case state == Sending:
		return "in_flight"
	case state == Queued && row.S("dispatch_evidence") == HostLostTurn:
		return "redelivering:" + HostLostTurn
	case state == Queued && str(pacing, "reason") == HourlyCap:
		return "awaiting_send:" + HourlyCap
	case state == Queued:
		return "awaiting_send"
	}
	return "awaiting_receipt"
}

// SnapshotItem is one delivery's operator view: the raw state, the reported state and phase.
func (d *Service) SnapshotItem(ctx context.Context, eventID string) (Obj, error) {
	row, err := d.Get(ctx, eventID)
	if err != nil {
		return nil, err
	}
	attempts, err := all(ctx, d.Store, "SELECT request_id, attempt_no, internal_state, state, affirmative_evidence, operation_observation, recipient_scan, record FROM attempts WHERE event_id = ? ORDER BY attempt_no", eventID)
	if err != nil {
		return nil, err
	}
	ack, err := one(ctx, d.Store, "SELECT * FROM acks WHERE event_id = ?", eventID)
	if err != nil {
		return nil, err
	}
	failure, err := one(ctx, d.Store, "SELECT * FROM failed_operations WHERE scope_key = ? ORDER BY occurred_at DESC", eventID)
	if err != nil {
		return nil, err
	}
	note, err := one(ctx, d.Store, "SELECT reason, noted_at FROM delivery_supersession WHERE event_id = ?", eventID)
	if err != nil {
		return nil, err
	}
	grant := ""
	if row.S("kind") == MergeTurnGrant {
		if grant, err = d.SupersessionReason(ctx, eventID); err != nil {
			return nil, err
		}
	}
	var pacing Obj
	if (row.S("state") == Queued || row.S("state") == DeferredBusy || row.S("state") == WithheldPreSend) && row.S("hold_reason") == "" {
		if pacing, err = d.pacing(ctx, row.S("recipient_task_id"), d.Clock.Now()); err != nil {
			return nil, err
		}
		if reopens, _ := get(pacing, "reopensAt"); pacing != nil && reopens != nil && !row.N("next_eligible_at") && row.F("next_eligible_at") > reopens.(float64) {
			pacing = nil
		}
	}
	detail := make([]any, len(attempts))
	for i, a := range attempts {
		detail[i] = a
	}
	var pacingValue any
	if pacing != nil {
		pacingValue = pacing
	}
	var noteObj any
	if note != nil {
		noteObj = Obj{{Key: "reason", Value: note.S("reason")}, {Key: "noted_at", Value: note.S("noted_at")}}
	}
	return Obj{
		{Key: "eventId", Value: eventID}, {Key: "kind", Value: row.S("kind")}, {Key: "recipient", Value: row.S("recipient_task_id")},
		{Key: "state", Value: row.S("state")}, {Key: "reported", Value: reportedState(row, ack, grant, note)},
		{Key: "attempts", Value: row.I("attempt_count")}, {Key: "holdReason", Value: row.Opt("hold_reason")},
		{Key: "nextEligibleAt", Value: row.Opt("next_eligible_at")}, {Key: "dispatchEvidence", Value: row.Opt("dispatch_evidence")},
		{Key: "acknowledged", Value: ack != nil}, {Key: "attemptDetail", Value: detail}, {Key: "phase", Value: phase(row, attempts, ack, failure, grant, note, pacing)}, {Key: "pacing", Value: pacingValue},
		{Key: "supersededNote", Value: noteObj},
	}, nil
}

// AttemptMessages is attempt_messages: every attempt's bytes with the status those bytes
// actually reached, read from the attempt and never inferred from the bytes existing.
func (d *Service) AttemptMessages(ctx context.Context, eventID string) ([]any, error) {
	rows, err := all(ctx, d.Store, "SELECT a.request_id, a.attempt_no, a.internal_state, a.state, a.record, a.sent_at, m.message FROM attempts a LEFT JOIN attempt_messages m ON m.request_id = a.request_id WHERE a.event_id = ? ORDER BY a.attempt_no", eventID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(rows))
	for _, row := range rows {
		var record Obj
		if row.S("record") != "" {
			record = loadsObj(row.S("record"))
		}
		status := "uncertain"
		switch state := row.S("state"); {
		case row.N("message"):
			status = "unavailable"
		case row.S("internal_state") != "settled":
			status = "prepared"
		case state == Dispatched || state == Acknowledged:
			status = Dispatched
		case state == HostLostTurn:
			status = HostLostTurn
		case record != nil && str(record, "sendAttempted") == "no":
			status = "confirmed_unsent"
		}
		out = append(out, Obj{{Key: "requestId", Value: row.S("request_id")}, {Key: "attemptNo", Value: row.I("attempt_no")}, {Key: "status", Value: status},
			{Key: "deliveryState", Value: row.Opt("state")}, {Key: "sentAt", Value: row.Opt("sent_at")}, {Key: "message", Value: row.Opt("message")}})
	}
	return out, nil
}
