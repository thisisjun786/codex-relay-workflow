package supervisor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Selection is supervision.select: reporting and standing are independent answers.
func (c *Channel) Selection(ctx context.Context, o Obligation, recipient string, now *float64) (map[string]any, error) {
	event := o.Subject
	if v, ok := o.Basis["eventId"].(string); ok && v != "" {
		event = v
	}
	rows, err := c.Store.All(ctx, "SELECT sync_id,state,target,target_ref,external_ref,confirmed_at,last_error FROM sync_outbox WHERE relationship_id = ? AND event_id = ? AND subject_kind = 'verdict' AND target = 'coordination_document' ORDER BY rowid", o.RelationID, event)
	if err != nil {
		return nil, err
	}
	records := make([]any, 0, len(rows))
	for _, row := range rows {
		records = append(records, map[string]any{"sync_id": row.Get("sync_id"), "state": row.Get("state"), "target": row.Get("target"), "target_ref": row.Get("target_ref"), "external_ref": row.Get("external_ref"), "confirmed_at": row.Get("confirmed_at"), "last_error": row.Get("last_error")})
	}
	var target string
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT target_ref FROM sync_targets WHERE relationship_id = ? AND target = 'coordination_document'", o.RelationID).Scan(&target)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	standing := "standing"
	discharge := "no coordination_document target is configured for this relationship, so there is no record the supervisor reads"
	if err == nil {
		discharge = "no Linear record was enqueued for this, so nothing the supervisor reads carries it"
		if len(records) > 0 {
			latest := records[len(records)-1].(map[string]any)
			if latest["target_ref"] != target {
				discharge = fmt.Sprintf("the current ruling's record is on %s, which is no longer the target for this relationship", latest["target_ref"].(string))
			} else if latest["state"] == "confirmed" {
				standing = "discharged"
				discharge = "the Linear record is confirmed"
			} else {
				discharge = fmt.Sprintf("the Linear record is %v rather than confirmed", latest["state"])
			}
		}
	}
	prior, err := c.PriorReport(ctx, o.ID)
	if err != nil {
		return nil, err
	}
	contact, err := c.contactability(ctx, recipient, now)
	if err != nil {
		return nil, err
	}
	decision := map[string]any{"schema": "supervisor-obligation/1", "obligationId": o.ID, "kind": o.Kind, "standing": standing, "dischargeReason": discharge, "priorReport": prior, "recipient": contact}
	switch {
	case standing == "discharged":
		decision["report"] = false
		decision["reason"] = "already_in_the_record_the_supervisor_reads"
	case prior != nil:
		decision["report"] = false
		decision["reason"] = "already_reported_under_this_obligation"
	case contact["contactable"] == nil:
		if contact["asked"] == false {
			decision["report"] = true
			decision["reason"] = "deliverability_was_not_asked_about"
		} else {
			decision["report"] = false
			decision["reason"] = "recipient_contactability_unmeasured"
		}
	case contact["contactable"] == false:
		decision["report"] = false
		decision["reason"] = "recipient_is_not_contactable"
	default:
		decision["report"] = true
		decision["reason"] = "reportable"
	}
	return decision, nil
}
func (c *Channel) contactability(ctx context.Context, recipient string, now *float64) (map[string]any, error) {
	if recipient == "" {
		return map[string]any{"contactable": nil, "asked": false, "reason": "no recipient was named, so deliverability was not part of this question"}, nil
	}
	var deliverable, observed string
	var withhold sql.NullString
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT deliverable,withhold_reason,observed_at FROM recipient_lifecycle WHERE task_id = ?", recipient).Scan(&deliverable, &withhold, &observed)
	if errors.Is(err, sql.ErrNoRows) {
		return map[string]any{"contactable": nil, "reason": "the host has not been observed for this task, so deliverability is unmeasured rather than allowed"}, nil
	}
	if err != nil {
		return nil, err
	}
	result := map[string]any{"deliverable": deliverable, "observedAt": observed}
	if deliverable != "yes" {
		reason := deliverable
		if withhold.Valid && withhold.String != "" {
			reason = withhold.String
		}
		result["contactable"] = false
		result["reason"] = reason
		return result, nil
	}
	moment, err := time.Parse(time.RFC3339Nano, observed)
	if err != nil {
		result["contactable"] = nil
		result["reason"] = "the observation carries no readable time, so its age is unmeasured"
		return result, nil
	}
	if now == nil {
		result["contactable"] = nil
		result["reason"] = "no clock was supplied, so whether this observation is still current is unmeasured"
		return result, nil
	}
	age := *now - float64(moment.UnixMicro())/1e6
	result["ageSeconds"] = age
	switch {
	case age < -60:
		result["contactable"] = nil
		result["reason"] = fmt.Sprintf("the observation is dated %ds in the future, so it is not evidence about now", int(-age))
	case age > 900:
		result["contactable"] = nil
		result["reason"] = fmt.Sprintf("the observation is %ds old, past the 900s this reading treats as current", int(age))
	default:
		result["contactable"] = true
		result["reason"] = deliverable
	}
	return result, nil
}
func (c *Channel) SelectEvent(ctx context.Context, eventID, recipient string, now float64) (map[string]any, error) {
	o, err := c.FromEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return map[string]any{"schema": "supervisor-obligation/1", "report": false, "reason": "no_meaningful_transition", "obligationId": nil, "detail": "this event is not a completion, a new block or a decision the user owes", "candidate": eventID}, nil
	}
	result, err := c.Selection(ctx, *o, recipient, &now)
	if err != nil {
		return nil, err
	}
	result["obligation"] = o
	return result, nil
}
