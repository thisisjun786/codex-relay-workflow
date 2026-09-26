package delivery

import (
	"context"
	"fmt"
	"strings"
)

// Anchored is assignment.AssignmentView._anchored for the fields this package owns: the event,
// its delivery and current attempt, the acknowledgement's two axes and the supersession note,
// read in one statement. The settings-hold reading and the undelivered reason of a withheld
// delivery are the assignment view's (todo 25); a delivery in such a state is refused here
// rather than reported without them.
func Anchored(ctx context.Context, d *Service, eventID any, generation int64) (Obj, error) {
	record := Obj{{Key: "eventId", Value: eventID}, {Key: "executionGeneration", Value: generation}, {Key: "event", Value: nil}, {Key: "delivery", Value: nil}, {Key: "ack", Value: nil}, {Key: "undeliveredReason", Value: nil}, {Key: "supersession", Value: nil}}
	if eventID == nil {
		return append(record, F{Key: "detail", Value: "this generation has no such event"}), nil
	}
	now := d.Clock.Now()
	window, earliest := d.Policy.RateWindows(now)
	row, err := one(ctx, d.Store, `SELECT e.stage AS stage, d.event_id AS delivered, d.state AS delivery_state, d.hold_reason AS hold_reason, d.dispatch_evidence AS dispatch_evidence, d.next_eligible_at AS next_eligible_at,
 a.request_id AS request_id, a.attempt_no AS attempt_no, a.state AS attempt_state, a.recipient_scan AS attempt_turn_check, v.last_reason AS ack_last_reason,
 (SELECT COUNT(*) FROM attempts h WHERE h.event_id = e.event_id AND h.state = 'host_lost_turn') AS host_lost_attempts,
 (SELECT sends FROM recipient_rate WHERE recipient_task_id = d.recipient_task_id AND window_start = ?) AS rate_sends,
 (SELECT MAX(last_send_at) FROM recipient_rate WHERE recipient_task_id = d.recipient_task_id AND window_start BETWEEN ? AND ?) AS rate_last,
 k.event_id AS acked, k.verified AS ack_verified, k.accepted AS ack_accepted, k.rejection_reason AS ack_rejection, v.tier AS ack_tier,
 r.status AS relationship_status, r.superseded_by AS superseded_by, sx.reason AS supersession_reason, sx.applied AS supersession_applied,
 (SELECT reason FROM refusals WHERE event_id = e.event_id ORDER BY id DESC LIMIT 1) AS refusal_reason
 FROM events e LEFT JOIN deliveries d ON d.event_id = e.event_id LEFT JOIN attempts a ON a.event_id = e.event_id AND a.attempt_no = d.attempt_count
 LEFT JOIN acks k ON k.event_id = e.event_id LEFT JOIN ack_evidence v ON v.event_id = e.event_id LEFT JOIN delivery_supersession sx ON sx.event_id = e.event_id
 LEFT JOIN relationships r ON r.relationship_id = e.relationship_id WHERE e.event_id = ?`, window, earliest, window, eventID)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return append(record, F{Key: "detail", Value: "the store holds no such event"}), nil
	}
	record = set(record, "event", Obj{{Key: "stage", Value: row.S("stage")}})
	state := row.S("delivery_state")
	delivered := !row.N("delivered")
	if delivered {
		if state == WithheldPreSend || state == InboxOnly {
			return nil, fmt.Errorf("delivery: the settings-hold reading of a %s delivery is the assignment view's (todo 25)", state)
		}
		var turnCheck any
		check := row.S("attempt_turn_check")
		for _, prefix := range []string{"turn_check_undecided:", "unknown_send_lost:", "unknown_send_undecided:"} {
			if strings.HasPrefix(check, prefix) {
				turnCheck = check
			}
		}
		var pacing any
		if (state == Queued || state == DeferredBusy) && row.S("hold_reason") == "" {
			var last *float64
			if !row.N("rate_last") {
				v := row.F("rate_last")
				last = &v
			}
			p := d.Policy.Pacing(now, row.I("rate_sends"), last)
			if reopens, _ := get(p, "reopensAt"); p != nil && (reopens == nil || row.N("next_eligible_at") || row.F("next_eligible_at") <= reopens.(float64)) {
				pacing = p
			}
		}
		record = set(record, "delivery", Obj{{Key: "state", Value: state}, {Key: "requestId", Value: row.Opt("request_id")}, {Key: "attemptNo", Value: row.Opt("attempt_no")}, {Key: "attemptState", Value: row.Opt("attempt_state")},
			{Key: "dispatchEvidence", Value: row.Opt("dispatch_evidence")}, {Key: "holdReason", Value: row.Opt("hold_reason")}, {Key: "hostLostAttempts", Value: row.I("host_lost_attempts")}, {Key: "turnCheck", Value: turnCheck}, {Key: "pacing", Value: pacing}, {Key: "settingsHold", Value: nil}})
	}
	acked := !row.N("acked")
	pick := func(v any) any {
		if acked {
			return v
		}
		return nil
	}
	tier := any("unrecorded")
	if !row.N("ack_tier") {
		tier = row.S("ack_tier")
	}
	record = set(record, "ack", Obj{{Key: "accepted", Value: pick(row.I("ack_accepted") != 0)}, {Key: "rejectionReason", Value: pick(row.Opt("ack_rejection"))}, {Key: "settlement", Value: pick(row.Opt("ack_verified"))}, {Key: "lastReason", Value: pick(row.Opt("ack_last_reason"))}, {Key: "evidenceTier", Value: tier}})
	var undelivered any
	switch {
	case delivered && row.S("hold_reason") != "":
		undelivered = Obj{{Key: "source", Value: "deliveries.hold_reason"}, {Key: "value", Value: row.S("hold_reason")}}
	case delivered && (state == Queued || state == DeferredBusy) && !row.N("relationship_status") && row.S("relationship_status") != "active" && row.N("superseded_by"):
		undelivered = Obj{{Key: "source", Value: "relationships.status"}, {Key: "value", Value: RelationshipNotActive}, {Key: "relationshipStatus", Value: row.S("relationship_status")}}
	case !delivered && !row.N("refusal_reason"):
		undelivered = Obj{{Key: "source", Value: "refusals.reason"}, {Key: "value", Value: row.S("refusal_reason")}}
	}
	record = set(record, "undeliveredReason", undelivered)
	if !row.N("supersession_reason") {
		record = set(record, "supersession", Obj{{Key: "reason", Value: row.S("supersession_reason")}, {Key: "applied", Value: row.I("supersession_applied") != 0}})
	}
	return record, nil
}

// Projection is the completion and correction anchors of the current generation.
func Projection(ctx context.Context, d *Service, rid string) (Obj, error) {
	r, err := LoadRelationship(ctx, d.Store, rid)
	if err != nil {
		return nil, err
	}
	head, err := HeadRevision(ctx, d.Store, rid, r.Generation)
	if err != nil {
		return nil, err
	}
	headID, _ := get(head, "eventId")
	completion, err := Anchored(ctx, d, headID, r.Generation)
	if err != nil {
		return nil, err
	}
	correction, err := one(ctx, d.Store, "SELECT event_id FROM events WHERE relationship_id = ? AND execution_generation = ? AND outcome = 'revision_request' AND suppressed_reason IS NULL ORDER BY event_id LIMIT 1", rid, r.Generation)
	if err != nil {
		return nil, err
	}
	var correctionID any
	if correction != nil {
		correctionID = correction.S("event_id")
	}
	anchored, err := Anchored(ctx, d, correctionID, r.Generation)
	if err != nil {
		return nil, err
	}
	return Obj{{Key: "completion", Value: completion}, {Key: "correction", Value: anchored}}, nil
}
