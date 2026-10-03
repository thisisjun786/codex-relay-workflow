package supervisor

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type StageResult map[string]any

func nullString(s string) sql.NullString { return sql.NullString{String: s, Valid: s != ""} }
func messageMap(r store.SupervisorMessagesRow) map[string]any {
	optionalString := func(v sql.NullString) any {
		if v.Valid {
			return v.String
		}
		return nil
	}
	optionalFloat := func(v sql.NullFloat64) any {
		if v.Valid {
			return v.Float64
		}
		return nil
	}
	optionalInt := func(v sql.NullInt64) any {
		if v.Valid {
			return v.Int64
		}
		return nil
	}
	return map[string]any{"message_id": r.MessageID, "obligation_id": r.ObligationID, "obligation_kind": r.ObligationKind, "relationship_id": r.RelationshipID, "project_key": optionalString(r.ProjectKey), "purpose": r.Purpose, "kind": r.Kind, "sender_task_id": r.SenderTaskID, "recipient_task_id": r.RecipientTaskID, "subject": r.Subject, "packet": r.Packet, "state": r.State, "attempt_count": r.AttemptCount, "next_eligible_at": optionalFloat(r.NextEligibleAt), "hold_reason": optionalString(r.HoldReason), "lease_owner": optionalString(r.LeaseOwner), "lease_until": optionalFloat(r.LeaseUntil), "staged_at": r.StagedAt, "updated_at": r.UpdatedAt, "event_id": optionalString(r.EventID), "submission_no": optionalInt(r.SubmissionNo), "reading": optionalString(r.Reading)}
}
func canonicalPacket(p Packet) (string, error) {
	raw, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err = decoder.Decode(&value); err != nil {
		return "", err
	}
	return pythonJSONSorted(value), nil
}

// pythonJSONSorted is json.dumps(sort_keys=True, ensure_ascii=False) for packet rows.
func pythonJSONSorted(value any) string {
	return pyjson.Dumps(value, pyjson.Options{SortKeys: true, Unicode: true})
}
func (c *Channel) Stage(ctx context.Context, o Obligation, expectRecipient, at string) (StageResult, error) {
	return c.StageWithReading(ctx, o, nil, expectRecipient, at)
}

func (c *Channel) StageWithReading(ctx context.Context, o Obligation, reading map[string]any, expectRecipient, at string) (StageResult, error) {
	if o.Kind == "blocked" || o.Kind == "decision_request" {
		latest, err := c.latestStatement(ctx, o)
		if err != nil {
			return nil, err
		}
		o = latest
	}
	if o.Kind == "unreported" {
		if _, ok := observationSelectors(reading); !ok {
			return nil, Refusal{"malformed_receipt", "an omission is staged with the reporting-observation/1 reading it came from: the obligation names a relationship and a turn, and the command that found it also needs the state directory, marker root, workspace, assignment and session. Without them the report would carry an evidence line nobody can follow"}
		}
		if err := validateObservation(o, reading, c.StoreDirectory()); err != nil {
			return nil, err
		}
	}
	if at == "" {
		at = time.Now().UTC().Format("2006-01-02T15:04:05.000000+00:00")
	}
	r, err := c.ResolveRecipient(ctx, o.RelationID, expectRecipient)
	if err != nil {
		return nil, err
	}
	if err = validateObligation(ctx, c, o); err != nil {
		return nil, err
	}
	p, err := c.composeReading(ctx, o, r, at, reading)
	if err != nil {
		return nil, err
	}
	encoded, err := canonicalPacket(p)
	if err != nil {
		return nil, err
	}
	id := p.ID()
	eventID, _ := o.Basis["eventId"].(string)
	report, err := currentReport(ctx, c.Store, eventID)
	if err != nil {
		return nil, err
	}
	var result StageResult
	if c.beforeStageLock != nil {
		c.beforeStageLock()
	}
	err = c.Store.Compose(ctx, func(tx context.Context, _ *sql.Conn) error {
		live, err := c.Resolve(tx, o.RelationID)
		if err != nil {
			return err
		}
		if live != r {
			return Refusal{"relation_owner_drift", "the hierarchy moved while this was being decided: it was read as " + strconv.Quote(r.Sender) + " reporting to " + strconv.Quote(r.Recipient) + " and changed under the write lock; nothing was staged"}
		}
		if err = validateObligation(tx, c, o); err != nil {
			return err
		}
		if o.Kind == "unreported" {
			if err = validateObservation(o, reading, c.StoreDirectory()); err != nil {
				return err
			}
			if withdrawn := c.omissionWithdrawn(tx, o, reading, at); withdrawn != "" {
				return Refusal{"not_claimable", "nothing is owed upward for omission " + strconv.Quote(o.ID) + ": " + withdrawn + ". Decided under the staging write lock; nothing was written"}
			}
		}
		fresh, err := currentReport(tx, c.Store, eventID)
		if err != nil {
			return err
		}
		freshPacket, err := c.composeReading(tx, o, live, at, reading)
		if err != nil {
			return err
		}
		freshEncoded, err := canonicalPacket(freshPacket)
		if err != nil {
			return err
		}
		if fresh != report || freshEncoded != encoded {
			return Refusal{"superseded_revision", "the work report for event " + strconv.Quote(eventID) + " changed while this was being staged; nothing was written; stage it again from the report that stands"}
		}
		existing, err := c.Store.SupervisorMessage(tx, id)
		if err == nil {
			if existing.HoldReason.String == store.SupervisorHoldSuperseded {
				if err = c.reopenProposal(tx, existing, at); err != nil {
					return err
				}
				existing, err = c.Store.SupervisorMessage(tx, id)
				if err != nil {
					return err
				}
			}
			if existing.HoldReason.String == store.SupervisorHoldUnaddressed && existing.SenderTaskID == live.Sender && existing.RecipientTaskID == live.Recipient && existing.Unsent() {
				if _, err = c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL,updated_at=? WHERE message_id=?", at, id); err != nil {
					return err
				}
				detail := pyjson.Dumps(contract.OrderedObject{{Key: "proposal", Value: "addressed"}, {Key: "reason", Value: unaddressedReleasedReason}}, pyjson.Options{})
				if _, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_reopened',?,?)", at, id, string(detail)); err != nil {
					return err
				}
				existing, err = c.Store.SupervisorMessage(tx, id)
				if err != nil {
					return err
				}
			}
			if o.Kind == "unreported" && existing.Reading.Valid {
				var frozen map[string]any
				decoder := json.NewDecoder(strings.NewReader(existing.Reading.String))
				decoder.UseNumber()
				if err := decoder.Decode(&frozen); err != nil {
					return err
				}
				if frozenObligation := ObservationObligation(frozen); frozenObligation == nil || observationReadingKey(*frozenObligation, frozen) != observationReadingKey(o, reading) {
					return Refusal{"contradictory_observation", "this omission is already staged as " + strconv.Quote(id) + " from another reading of it, which disagrees with this one about what it is or where it can be read. The staged message keeps the reading it froze; nothing was written"}
				}
			}
			var submission sql.NullInt64
			if report.submission != 0 {
				submission = sql.NullInt64{Int64: report.submission, Valid: true}
			}
			restating := existing.EventID != nullString(eventID) || existing.SubmissionNo != submission
			if existing.SenderTaskID != r.Sender || existing.RecipientTaskID != r.Recipient {
				if !existing.Unsent() {
					return Refusal{"relation_owner_drift", "message " + strconv.Quote(id) + " was staged from " + strconv.Quote(existing.SenderTaskID) + " to " + strconv.Quote(existing.RecipientTaskID) + " and has " + fmt.Sprint(existing.AttemptCount) + " attempt(s), state " + strconv.Quote(existing.State) + "; the linkage now says " + strconv.Quote(r.Sender) + " reports to " + strconv.Quote(r.Recipient) + ". A message an attempt may have sent is never re-addressed, because its attempts would then describe a recipient they were never sent to - it went to the supervisor who was live when its transport started - so " + strconv.Quote(r.Recipient) + " has not been told by this channel, and the obligation stands until the Linear record confirms it"}
				}
				var unsafe int
				if err = c.Store.Q(tx).QueryRowContext(tx, "SELECT COUNT(*) FROM supervisor_attempts WHERE message_id=? AND "+store.SupervisorAttemptMayHaveGoneSQL(""), id).Scan(&unsafe); err != nil {
					return err
				}
				if unsafe != 0 {
					return Refusal{"relation_owner_drift", "message " + strconv.Quote(id) + " was already attempted and is never re-addressed"}
				}
				update, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET sender_task_id=?,recipient_task_id=?,project_key=?,packet=?,event_id=?,submission_no=?,state='queued',next_eligible_at=NULL,hold_reason=NULL,updated_at=? WHERE message_id=? AND sender_task_id=? AND recipient_task_id=? AND "+store.SupervisorNeverSentSQL(), r.Sender, r.Recipient, r.ProjectKey, encoded, nullString(eventID), submission, at, id, existing.SenderTaskID, existing.RecipientTaskID)
				if err != nil {
					return err
				}
				moved, err := update.RowsAffected()
				if err != nil {
					return err
				}
				if moved != 1 {
					return Refusal{"relation_owner_drift", "message moved before it could be re-addressed"}
				}
				from := map[string]any{"sender": existing.SenderTaskID, "recipient": existing.RecipientTaskID, "projectKey": optionalText(existing.ProjectKey)}
				to := map[string]any{"sender": r.Sender, "recipient": r.Recipient, "projectKey": r.ProjectKey}
				detail := pyjson.Dumps(contract.OrderedObject{{Key: "from", Value: contract.OrderedObject{{Key: "sender", Value: from["sender"]}, {Key: "recipient", Value: from["recipient"]}, {Key: "projectKey", Value: from["projectKey"]}}}, {Key: "to", Value: contract.OrderedObject{{Key: "sender", Value: to["sender"]}, {Key: "recipient", Value: to["recipient"]}, {Key: "projectKey", Value: to["projectKey"]}}}, {Key: "releasedHold", Value: optionalText(existing.HoldReason)}, {Key: "releasedState", Value: existing.State}, {Key: "fromEvent", Value: optionalText(existing.EventID)}, {Key: "toEvent", Value: optionalText(nullString(eventID))}, {Key: "reason", Value: "the hierarchy moved before anything was sent"}}, pyjson.Options{})
				if _, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_readdressed',?,?)", at, id, string(detail)); err != nil {
					return err
				}
				current, err := c.Store.SupervisorMessage(tx, id)
				if err != nil {
					return err
				}
				result = StageResult{"schema": channelVersion, "staged": false, "readdressed": true, "restated": restating, "messageId": id, "from": from, "to": to, "fromEvent": optionalText(existing.EventID), "toEvent": optionalText(nullString(eventID)), "reason": "staged for a hierarchy that has since moved, and never attempted, so it now goes to the live supervisor instead", "message": messageMap(current), "recipient": r.Recipient, "sender": r.Sender}
				return nil
			}
			if restating {
				update, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET packet=?,event_id=?,submission_no=?,updated_at=? WHERE message_id=? AND "+store.SupervisorNeverSentSQL(), encoded, nullString(eventID), submission, at, id)
				if err != nil {
					return err
				}
				moved, err := update.RowsAffected()
				if err != nil {
					return err
				}
				if moved == 1 {
					detail := pyjson.Dumps(contract.OrderedObject{{Key: "fromEvent", Value: optionalText(existing.EventID)}, {Key: "toEvent", Value: optionalText(nullString(eventID))}, {Key: "fromSubmission", Value: optionalNumber(existing.SubmissionNo)}, {Key: "toSubmission", Value: optionalNumber(submission)}, {Key: "reason", Value: "the child stated this " + o.Kind + " again before anything was sent, so the message now carries that statement"}}, pyjson.Options{})
					if _, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_restated',?,?)", at, id, detail); err != nil {
						return err
					}
					updated, err := c.Store.SupervisorMessage(tx, id)
					if err != nil {
						return err
					}
					from := map[string]any{"sender": existing.SenderTaskID, "recipient": existing.RecipientTaskID, "projectKey": optionalText(existing.ProjectKey)}
					result = StageResult{"schema": channelVersion, "staged": false, "readdressed": false, "restated": true, "messageId": id, "from": from, "to": from, "fromEvent": optionalText(existing.EventID), "toEvent": optionalText(nullString(eventID)), "message": messageMap(updated), "recipient": r.Recipient, "sender": r.Sender, "reason": "stated again before anything was sent, so the message now carries the newest statement"}
					return nil
				}
			}
			result = StageResult{"schema": channelVersion, "staged": false, "messageId": id, "reason": "this fact is already staged; one obligation is one message", "message": messageMap(existing)}
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		reportable, reason, err := c.Reportable(tx, o)
		if err != nil {
			return err
		}
		if !reportable {
			return Refusal{"not_claimable", "nothing is owed upward for obligation " + strconv.Quote(o.ID) + ": " + reason + ", decided under the staging write lock. The obligation is preserved either way; what is refused is producing a second report about a fact somebody has already reported or the supervisor can already read for itself"}
		}
		row := store.SupervisorMessagesRow{MessageID: id, ObligationID: o.ID, ObligationKind: o.Kind, RelationshipID: o.RelationID, ProjectKey: nullString(r.ProjectKey), Purpose: purpose(o.Kind), Kind: packetKind(o.Kind), SenderTaskID: r.Sender, RecipientTaskID: r.Recipient, Subject: o.Subject, Packet: encoded, State: "queued", StagedAt: at, UpdatedAt: at, EventID: nullString(eventID)}
		if reading != nil {
			row.Reading = nullString(pyjson.Dumps(reading, pyjson.Options{SortKeys: true, Unicode: true}))
		}
		if report.submission != 0 {
			row.SubmissionNo = sql.NullInt64{Int64: report.submission, Valid: true}
		}
		staged, err := c.Store.StageSupervisorMessage(tx, row)
		if err != nil {
			return err
		}
		if staged {
			detail := pyjson.Dumps(contract.OrderedObject{{Key: "kind", Value: o.Kind}, {Key: "relationId", Value: o.RelationID}, {Key: "subject", Value: o.Subject}, {Key: "messageId", Value: id}, {Key: "note", Value: "staged on the supervisor channel"}}, pyjson.Options{})
			_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,'supervisor_report',?,?)", at, o.ID, string(detail))
			if err != nil {
				return err
			}
		}
		persisted, err := c.Store.SupervisorMessage(tx, id)
		if err != nil {
			return err
		}
		why := "staged"
		if !staged {
			why = "another caller staged this fact first"
		}
		result = StageResult{"schema": channelVersion, "staged": staged, "messageId": id, "reason": why, "message": messageMap(persisted), "recipient": r.Recipient, "sender": r.Sender}
		return nil
	})
	return result, err
}
func (c *Channel) Get(ctx context.Context, id string) (store.SupervisorMessagesRow, error) {
	row, err := c.Store.SupervisorMessage(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return row, Refusal{"not_claimable", "no supervisor message is staged as " + pyvalue.StrRepr(id)}
	}
	return row, err
}
func (c *Channel) StageStanding(ctx context.Context, projectKey, at string) (map[string]any, error) {
	return c.StageStandingWithObservations(ctx, projectKey, nil, at)
}

func obligationFromStanding(one map[string]any) Obligation {
	o := Obligation{
		Schema:     pyvalue.Str(one["schema"]),
		ID:         pyvalue.Str(one["obligationId"]),
		Kind:       pyvalue.Str(one["kind"]),
		RelationID: pyvalue.Str(one["relationId"]),
		Subject:    pyvalue.Str(one["subject"]),
		Generation: one["executionGeneration"],
		Basis:      one["basis"].(map[string]any),
		Detail:     pyvalue.Str(one["detail"]),
	}
	if revision, ok := one["revisionHash"].(string); ok {
		o.Revision = &revision
	}
	if issue, ok := one["issueKey"].(string); ok {
		o.Issue = &issue
	}
	return o
}

func (c *Channel) StageStandingWithObservations(ctx context.Context, projectKey string, observations []map[string]any, at string) (map[string]any, error) {
	values := make([]any, len(observations))
	for i, reading := range observations {
		values[i] = reading
	}
	standing, err := c.Standing(ctx, projectKey, values)
	if err != nil {
		return nil, err
	}
	// Re-read the project scope at staging time, as Python's standing_for does. Besides keeping
	// the staging snapshot current, this preserves the project-wrap observation used by autosend.
	if _, err := c.Store.ScopedRelationships(ctx, projectKey); err != nil {
		return nil, err
	}
	readings := map[string]map[string]map[string]any{}
	for _, reading := range observations {
		o := ObservationObligation(reading)
		if o == nil {
			continue
		}
		if _, ok := observationSelectors(reading); !ok {
			continue
		}
		key := observationReadingKey(*o, reading)
		if readings[o.ID] == nil {
			readings[o.ID] = map[string]map[string]any{}
		}
		if _, exists := readings[o.ID][key]; !exists {
			readings[o.ID][key] = reading
		}
	}
	staged := []any{}
	refused := []any{}
	for _, value := range standing["standing"].([]any) {
		one := value.(map[string]any)
		o := obligationFromStanding(one)
		found := readings[o.ID]
		if len(found) > 1 {
			refused = append(refused, map[string]any{"obligationId": o.ID, "kind": o.Kind, "reason": "contradictory_observation", "detail": fmt.Sprintf("%d readings of this omission disagree about what it is or where it can be read, and a report carries exactly one; nothing was staged for it", len(found))})
			continue
		}
		var reading map[string]any
		for _, candidate := range found {
			reading = candidate
		}
		if raised := ObservationObligation(reading); raised != nil {
			o = *raised
		}
		answer, err := c.StageWithReading(ctx, o, reading, "", at)
		if refusal := new(Refusal); errors.As(err, refusal) {
			refused = append(refused, map[string]any{"obligationId": o.ID, "kind": o.Kind, "reason": refusal.Reason, "detail": refusal.Detail})
		} else if err != nil {
			return nil, err
		} else {
			staged = append(staged, answer)
		}
	}
	return map[string]any{"schema": channelVersion, "projectKey": projectKey, "staged": staged, "refused": refused, "gaps": standing["gaps"], "limits": "staging is not sending and sending is not reading. Each message here has to be sent and read back before anything says it arrived, and a readback shows arrival rather than that a supervisor read it"}, nil
}

func observationReadingKey(o Obligation, reading map[string]any) string {
	selectors, _ := observationSelectors(reading)
	return pyjson.Dumps(map[string]any{
		"obligation": map[string]any{"obligationId": o.ID, "kind": o.Kind, "relationId": o.RelationID, "subject": o.Subject, "executionGeneration": o.Generation, "revisionHash": o.Revision, "issueKey": o.Issue, "basis": o.Basis, "detail": o.Detail},
		"selectors":  selectors,
	}, pyjson.Options{SortKeys: true})
}
