package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// refreshProposal re-derives an unsent event's packet in the writer's transaction.
// The bytes are frozen only after the transport-start write commits.
func (c *Channel) latestStatement(ctx context.Context, o Obligation) (Obligation, error) {
	if o.Kind != "blocked" && o.Kind != "decision_request" {
		return o, nil
	}
	row := store.SupervisorMessagesRow{ObligationID: o.ID, ObligationKind: o.Kind, RelationshipID: o.RelationID, EventID: nullString(pyvalue.Str(o.Basis["eventId"]))}
	latest, err := c.newestRaising(ctx, row)
	if err != nil || latest == nil {
		return o, err
	}
	return *latest, nil
}

func (c *Channel) omissionWithdrawn(ctx context.Context, o Obligation, reading map[string]any, at string) string {
	derived := orderedMap(delivery.DeriveOmission(ctx, c.Store, c.StoreDirectory(), o.RelationID, o.Subject, at, 0))
	if derived["reason"] == delivery.OmittedDeclarationsMissing || derived["reason"] == "registration_unresolved" {
		if reading["source"] == delivery.OmittedStoreSource {
			return "this store no longer holds the claim record this omission was derived under, so it cannot derive it again"
		}
		return ""
	}
	if derived["reportingState"] != "unreported" || derived["owed"] != true {
		return "this store, which records this session's declarations, derives turn " + pyvalue.StrRepr(o.Subject) + " as " + pyvalue.StrRepr(pyvalue.Str(derived["reportingState"])) + " (" + pyvalue.Str(derived["reason"]) + ") and owed answers " + pyvalue.StrRepr(pyvalue.Str(derived["owedReason"])) + ", so nothing is owed through this message"
	}
	if reading["source"] == delivery.OmittedStoreSource && observationReadingKey(o, derived) != observationReadingKey(o, reading) {
		return "this store derives turn " + pyvalue.StrRepr(o.Subject) + "'s omission differently from the reading frozen on this message"
	}
	return ""
}

func (c *Channel) refreshProposal(ctx context.Context, row store.SupervisorMessagesRow, r Resolution, at string, currentAttempt int64) (changed bool, err error) {
	defer evidence.RecoverPython(&err)
	if row.ObligationKind == "fault_notification" {
		return c.refreshNotice(ctx, row, r, at, currentAttempt)
	}
	if !row.EventID.Valid {
		if row.Reading.Valid && row.ObligationKind == "unreported" {
			reading := map[string]any{}
			decoder := json.NewDecoder(strings.NewReader(row.Reading.String))
			decoder.UseNumber()
			if err := decoder.Decode(&reading); err != nil {
				return false, err
			}
			o := ObservationObligation(reading)
			if o != nil {
				if withdrawn := c.omissionWithdrawn(ctx, *o, reading, at); withdrawn != "" {
					return false, Refusal{"superseded_revision", withdrawn + heldSupersededTail}
				}
			}
			var event string
			err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT event_id FROM events WHERE relationship_id=? AND turn_id=? AND stage='final' ORDER BY rowid DESC LIMIT 1", row.RelationshipID, row.Subject).Scan(&event)
			if err == nil {
				return false, Refusal{"superseded_revision", "turn " + pyvalue.StrRepr(row.Subject) + " has a final receipt, event " + pyvalue.StrRepr(event) + ", accepted after this omission was staged; the turn reported, and what that event raises goes up as its own fact" + heldSupersededTail}
			}
			if err != sql.ErrNoRows {
				return false, err
			}
			// An omission's packet is composed from its frozen reading and the live resolution, and
			// the resolution's recipient scope kind can move without the recipient task moving (the
			// same task answering from the store seat instead of an initiative's), so a claim and a
			// transport start must not send the stale kind.
			if o != nil {
				return c.refreshEventlessProposal(ctx, row, r, at, currentAttempt)
			}
		}
		return false, nil
	}
	o, err := c.newestRaising(ctx, row)
	if err != nil {
		return false, err
	}
	if o == nil || o.ID != row.ObligationID {
		raised, err := c.FromEvent(ctx, row.EventID.String)
		if err != nil {
			return false, err
		}
		description := "nothing"
		if raised != nil {
			description = pyvalue.StrRepr(raised.Kind) + " obligation " + pyvalue.StrRepr(raised.ID)
		}
		return false, Refusal{"superseded_revision", "no event raises obligation " + pyvalue.StrRepr(row.ObligationID) + " any more; event " + pyvalue.StrRepr(row.EventID.String) + " now raises " + description + heldSupersededTail}
	}
	owed, _, err := c.ReportableCurrent(ctx, *o)
	if err != nil {
		return false, err
	}
	if !owed {
		return false, Refusal{"superseded_revision", "the tracking record now confirms this obligation, so the level above already has it" + heldSupersededTail}
	}
	eventID, _ := o.Basis["eventId"].(string)
	report, err := currentReport(ctx, c.Store, eventID)
	if err != nil {
		return false, err
	}
	old := evidence.Decode(row.Packet)
	observed := evidence.Dict(evidence.Item(old, "envelope"), false)["observedAt"]
	current, err := c.Compose(ctx, *o, r, at)
	if err != nil {
		return false, err
	}
	if observed != nil {
		current["envelope"].(map[string]any)["observedAt"] = observed
	}
	comparison, err := canonicalPacket(current)
	if err != nil {
		return false, err
	}
	// A packet stored before recipient.scopeKind existed carries no such field, so the freshly
	// composed one always differs. Compare without it there: adding the field alone is not a
	// change to the message, and a restatement made for something else still writes it.
	if !recipientScopeKindRecorded(row.Packet) {
		comparison, err = canonicalPacket(packetWithoutRecipientScopeKind(current))
		if err != nil {
			return false, err
		}
	}
	if comparison == row.Packet && eventID == row.EventID.String && (!row.SubmissionNo.Valid && report.submission == 0 || row.SubmissionNo.Valid && row.SubmissionNo.Int64 == report.submission) {
		return false, nil
	}
	// A restatement at claim or transport start is observed at that instant; stage-time
	// restatement is handled in StageWithReading and retains its staging instant.
	p, err := c.Compose(ctx, *o, r, at)
	if err != nil {
		return false, err
	}
	encoded, err := canonicalPacket(p)
	if err != nil {
		return false, err
	}
	var submission sql.NullInt64
	if report.submission != 0 {
		submission = sql.NullInt64{Int64: report.submission, Valid: true}
	}
	result, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE supervisor_messages SET packet=?,event_id=?,submission_no=?,updated_at=? WHERE message_id=? AND "+store.SupervisorRestatableSQL("")+" AND recipient_task_id=? AND NOT EXISTS (SELECT 1 FROM supervisor_attempts WHERE message_id=? AND attempt_no<>? AND "+store.SupervisorAttemptMayHaveGoneSQL("")+")", encoded, eventID, submission, at, row.MessageID, r.Recipient, row.MessageID, currentAttempt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, Refusal{"superseded_revision", "the report was sent; the staged packet cannot be changed"}
	}
	instant := "claim"
	if row.State == "sending" {
		instant = "transport_start"
	}
	detail := pyjson.Dumps(contract.OrderedObject{{Key: "fromEvent", Value: row.EventID.String}, {Key: "toEvent", Value: eventID}, {Key: "fromSubmission", Value: optionalNumber(row.SubmissionNo)}, {Key: "toSubmission", Value: optionalNumber(submission)}, {Key: "at", Value: instant}, {Key: "reason", Value: "what the obligation says, or the words the message is written in, moved after staging and nothing had been sent, so the message now carries what is owed now in the current words"}}, pyjson.Options{})
	_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_restated',?,?)", at, row.MessageID, detail)
	return true, err
}

// refreshEventlessProposal keeps an eventless omission's stored packet naming the seat its recipient
// answers from now. An omission carries no event, so its packet is not recomposed here: it was
// composed from the reading the omission was frozen with, and recomposing it would rewrite fields
// that have not moved. What can move is the resolution's recipient scope kind, which the same
// recipient task can change seat on (an initiative's supervisor handing the store seat, or the
// reverse), and a claim and a transport start must not send the stale kind. Only that one field is
// rewritten, in the stored packet's own bytes. A packet stored before the field existed is left
// exactly as it is: nothing about an old packet is guessed at, which is the same rule the event and
// notice paths follow.
func (c *Channel) refreshEventlessProposal(ctx context.Context, row store.SupervisorMessagesRow, r Resolution, at string, currentAttempt int64) (bool, error) {
	decoded, err := pyjson.Loads(row.Packet, pyjson.LoadOptions{Python: true, RangeErrors: true})
	if err != nil {
		return false, err
	}
	top, ok := evidence.Object(decoded)
	if !ok {
		return false, nil
	}
	envelope, _ := top.Lookup("envelope")
	region, ok := evidence.Object(envelope)
	if !ok {
		return false, nil
	}
	recipient, _ := region.Lookup("recipient")
	person, ok := evidence.Object(recipient)
	if !ok {
		return false, nil
	}
	stored, present := person.Lookup("scopeKind")
	if !present || stored == r.RecipientScopeKind {
		return false, nil
	}
	updated := person.Set("scopeKind", r.RecipientScopeKind)
	region = region.Set("recipient", updated)
	top = top.Set("envelope", region)
	encoded := pyjson.Dumps(top, pyjson.Options{SortKeys: true, Unicode: true})
	result, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE supervisor_messages SET packet=?,updated_at=? WHERE message_id=? AND "+store.SupervisorRestatableSQL("")+" AND recipient_task_id=? AND NOT EXISTS (SELECT 1 FROM supervisor_attempts WHERE message_id=? AND attempt_no<>? AND "+store.SupervisorAttemptMayHaveGoneSQL("")+")", encoded, at, row.MessageID, r.Recipient, row.MessageID, currentAttempt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return false, Refusal{"superseded_revision", "the report was sent; the staged packet cannot be changed"}
	}
	instant := "claim"
	if row.State == "sending" {
		instant = "transport_start"
	}
	detail := pyjson.Dumps(contract.OrderedObject{{Key: "at", Value: instant}, {Key: "reason", Value: "the recipient of this omission now answers from another scope, so the packet records the seat it is addressed to now"}}, pyjson.Options{})
	_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_restated',?,?)", at, row.MessageID, detail)
	return true, err
}

// ReportableCurrent checks discharge without treating the channel's own staged journal as a second report.
func (c *Channel) ReportableCurrent(ctx context.Context, o Obligation) (bool, string, error) {
	var target string
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT target_ref FROM sync_targets WHERE relationship_id=? AND target='coordination_document'", o.RelationID).Scan(&target)
	if err == sql.ErrNoRows {
		return true, "", nil
	}
	if err != nil {
		return false, "", err
	}
	var state, ref string
	err = c.Store.Q(ctx).QueryRowContext(ctx, "SELECT state,target_ref FROM sync_outbox WHERE relationship_id=? AND event_id=? AND subject_kind='verdict' AND target='coordination_document' ORDER BY rowid DESC LIMIT 1", o.RelationID, o.Basis["eventId"]).Scan(&state, &ref)
	if err != nil && err != sql.ErrNoRows {
		return false, "", err
	}
	if err == nil && state == "confirmed" && ref == target {
		return false, "already_in_the_record_the_supervisor_reads", nil
	}
	// a relationship a plan runs has no sync_outbox row of its verdict: the plan's confirmed summary entry is the record (delivery.PlanSummaryCarriesVerdict)
	eventID, _ := o.Basis["eventId"].(string)
	summary, err := delivery.PlanSummaryCarriesVerdict(ctx, c.Store.Q(ctx), o.RelationID, eventID, target)
	if err != nil {
		return false, "", err
	}
	if summary != "" {
		return false, "already_in_the_record_the_supervisor_reads", nil
	}
	return true, "", nil
}

func (c *Channel) newestRaising(ctx context.Context, row store.SupervisorMessagesRow) (*Obligation, error) {
	if row.ObligationKind != "blocked" && row.ObligationKind != "decision_request" {
		return c.FromEvent(ctx, row.EventID.String)
	}
	rows, err := c.Store.Q(ctx).QueryContext(ctx, "SELECT event_id FROM events WHERE relationship_id=? ORDER BY first_seen_at DESC,rowid DESC", row.RelationshipID)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err = rows.Scan(&id); err != nil {
			break
		}
		ids = append(ids, id)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	if err != nil {
		return nil, err
	}
	for _, id := range ids {
		o, e := c.FromEvent(ctx, id)
		if e != nil {
			return nil, e
		}
		if o != nil && o.ID == row.ObligationID {
			return o, nil
		}
	}
	return nil, nil
}

// The prose of the two holds, spelled through the store's hold words so that a sentence and the
// word it names cannot drift apart.
const (
	// heldSupersededTail ends every refusal that says a message was parked as superseded.
	heldSupersededTail = ". Nothing is sent through this message; it is held as '" + store.SupervisorHoldSuperseded + "', and staging the project stages what is owed now"
	// unaddressedReleasedReason is the journal reason of a message re-addressed to the live hierarchy.
	unaddressedReleasedReason = "the hierarchy names this message's endpoints again, so the " + store.SupervisorHoldUnaddressed + " hold is released"
	// supersededReleasedReason is the journal reason of a message whose proposal is owed through it again.
	supersededReleasedReason = "what this message is for is owed through it again, so the " + store.SupervisorHoldSuperseded + " hold is released"
	// acknowledgedNewerStatementReason is the staging answer for an acknowledged park kept through a
	// newer statement that is not owed now: the park keeps the statement its recipient acknowledged.
	acknowledgedNewerStatementReason = "this fact is already staged and its recipient's acknowledgement parked it; a newer statement of it is not owed through it now, so the message keeps the statement its recipient acknowledged"
)

func (c *Channel) holdSuperseded(ctx context.Context, id, requestID string, attemptNo int64, at, detail, owner string) error {
	record := pyjson.Dumps(map[string]any{"requestId": requestID, "messageId": id, "attemptNo": attemptNo, "deliveryState": "withheld_pre_send", "sendAttempted": "no", "retrySafe": true, "reason": "what is owed moved between the claim and the transport", "proposal": "obsolete", "detail": detail}, pyjson.Options{SortKeys: true})
	if err := c.Store.SettleSupervisorAttempt(ctx, requestID, "withheld_pre_send", "no", 1, sql.NullString{}, record, at); err != nil {
		return err
	}
	// The hold rule's derive entry: superseded_by_report is set only on a message with no hold, and
	// only that set journals supervisor_message_superseded, so the newest such row stays the one of
	// the write that set the hold the message carries (holdKindOf). A message that came to carry a
	// hold while it was sending is settled and keeps it, and nothing is journaled.
	current, err := c.Get(ctx, id)
	if err != nil {
		return err
	}
	kind, err := holdKindOf(ctx, c.Store, current)
	if err != nil {
		return err
	}
	verdict, _, err := holdDecision(kind, holdChange{Entry: entryDerive}, nil)
	if err != nil {
		return err
	}
	var hold sql.NullString
	if verdict == holdSet {
		hold = sql.NullString{String: store.SupervisorHoldSuperseded, Valid: true}
	}
	moved, err := c.Store.SettleSupervisorMessage(ctx, id, "queued", sql.NullFloat64{}, hold, at, "sending", attemptNo, sql.NullString{String: owner, Valid: true})
	if err != nil || !moved || verdict != holdSet {
		return err
	}
	_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_superseded',?,?)", at, id, pyjson.Dumps(contract.OrderedObject{{Key: "requestId", Value: requestID}, {Key: "detail", Value: detail}}, pyjson.Options{}))
	return err
}

func (c *Channel) reopenAddressed(ctx context.Context, row store.SupervisorMessagesRow, at string) error {
	return c.Store.Compose(ctx, func(tx context.Context, _ *sql.Conn) error {
		live, err := c.Get(tx, row.MessageID)
		if err != nil || live.HoldReason.String != store.SupervisorHoldUnaddressed {
			return err
		}
		r, err := c.ResolveFor(tx, live.RelationshipID, rowGrade(live))
		if err != nil || r.Sender != live.SenderTaskID || r.Recipient != live.RecipientTaskID || r.ProjectKey != live.ProjectKey.String {
			return err
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL,updated_at=? WHERE message_id=? AND hold_reason='"+store.SupervisorHoldUnaddressed+"' AND "+store.SupervisorUnsentSQL(""), at, row.MessageID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return err
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "proposal", Value: "addressed"}, {Key: "reason", Value: unaddressedReleasedReason}}, pyjson.Options{})
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_reopened',?,?)", at, row.MessageID, detail)
		return err
	})
}

// reopenProposal asks the hold rule (holdDecision) whether a message held superseded_by_report is
// released, as entry: restage when its obligation is staged again or the daemon attempts it,
// send_by_id when an attempt named it. It answers the rule's verdict and reason, and writes only a
// release the rule answers: an acknowledged park of the same statement is kept by a restage and
// nothing is written or journaled. ifOwed asks refreshProposal, which may restate the packet, as the
// release it allows would; a release the rule answers outright restates the packet first too, and
// keeps the hold when the proposal is no longer owed (dev's rule for those cells). A release because
// the hierarchy names another recipient is written by the staging's re-address, which asks the rule
// itself, so nothing is written here. A send by id whose endpoints moved is answered as the refusal
// relation_owner_drift: a send by id never re-addresses.
func (c *Channel) reopenProposal(ctx context.Context, row store.SupervisorMessagesRow, at string, entry holdEntry) (verdict holdVerdict, why string, err error) {
	verdict = holdKeep
	err = c.Store.Compose(ctx, func(tx context.Context, _ *sql.Conn) error {
		live, err := c.Get(tx, row.MessageID)
		if err != nil || live.HoldReason.String != store.SupervisorHoldSuperseded {
			return err
		}
		r, err := c.ResolveFor(tx, live.RelationshipID, rowGrade(live))
		if err != nil {
			return err
		}
		kind, err := holdKindOf(tx, c.Store, live)
		if err != nil {
			return err
		}
		change := holdChange{Entry: entry, SameRecipient: r.Recipient == live.RecipientTaskID, SameSender: r.Sender == live.SenderTaskID}
		if kind == holdAcknowledged && entry == entryRestage && change.SameRecipient {
			if change.SameStatement, err = c.staysTheSameStatement(tx, live, r, at); err != nil {
				return err
			}
		}
		asked := false
		owed := func() (bool, error) {
			asked = true
			_, err := c.refreshProposal(tx, live, r, at, 0)
			if refusal := new(Refusal); errors.As(err, refusal) && refusal.Reason == "superseded_revision" {
				return false, nil
			}
			return err == nil, err
		}
		verdict, why, err = holdDecision(kind, change, owed)
		if err != nil {
			return err
		}
		switch {
		case verdict == holdKeep && why == sendByIDDriftReason:
			return Refusal{"relation_owner_drift", why}
		case verdict != holdRelease:
			return nil
		case !change.SameRecipient:
			return nil
		}
		if !asked {
			if owes, err := owed(); err != nil || !owes {
				if err == nil {
					verdict, why = holdKeep, "what the message is for is not owed through it any more"
				}
				return err
			}
		}
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL,next_eligible_at=NULL,updated_at=? WHERE message_id=? AND hold_reason='"+store.SupervisorHoldSuperseded+"' AND "+store.SupervisorUnsentSQL(""), at, row.MessageID)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed != 1 {
			return err
		}
		detail := pyjson.Dumps(contract.OrderedObject{{Key: "proposal", Value: "current"}, {Key: "reason", Value: supersededReleasedReason}}, pyjson.Options{})
		_, err = c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_reopened',?,?)", at, row.MessageID, detail)
		return err
	})
	if err != nil {
		return holdKeep, "", err
	}
	return verdict, why, nil
}

// staysTheSameStatement reports whether what would be staged for row now, addressed through r, says
// what the stored row says (statementKey, plan-r3 section 1). The fresh side is composed the way
// staging composes it: an event's message from its newest raising obligation and the newest work
// report, an omission's from the reading it froze. A direct message and a fault notice are never
// recomposed from anything that moves, so the stored statement is the one that would be sent. A
// row whose event no longer raises its obligation, or whose packet cannot be composed now, is not the
// same statement (the rule then asks whether it is owed, and refreshProposal answers that).
func (c *Channel) staysTheSameStatement(ctx context.Context, row store.SupervisorMessagesRow, r Resolution, at string) (same bool, err error) {
	defer evidence.RecoverPython(&err)
	var columns statementColumns
	var fresh Packet
	switch {
	case IsDirectKind(row.ObligationKind) || row.ObligationKind == store.SupervisorNoticeObligationKind:
		return true, nil
	case row.EventID.Valid:
		o, err := c.newestRaising(ctx, row)
		if err != nil || o == nil || o.ID != row.ObligationID {
			return false, err
		}
		eventID, _ := o.Basis["eventId"].(string)
		report, err := currentReport(ctx, c.Store, eventID)
		if err != nil {
			return false, err
		}
		if fresh, err = c.composeReading(ctx, *o, r, at, nil); err != nil {
			return false, composeRefusalIsNotSame(err)
		}
		columns = obligationStatementColumns(*o, report.submission)
	case row.ObligationKind == "unreported" && row.Reading.Valid:
		reading := map[string]any{}
		decoder := json.NewDecoder(strings.NewReader(row.Reading.String))
		decoder.UseNumber()
		if err := decoder.Decode(&reading); err != nil {
			return false, err
		}
		// staging hands a frozen reading back with its generation as an integer (stageUnsentWithReadings)
		if generation, ok := reading["executionGeneration"].(json.Number); ok {
			n, err := generation.Int64()
			if err != nil {
				return false, err
			}
			reading["executionGeneration"] = n
		}
		o := ObservationObligation(reading)
		if o == nil {
			return true, nil
		}
		if fresh, err = c.composeReading(ctx, *o, r, at, reading); err != nil {
			return false, composeRefusalIsNotSame(err)
		}
		columns = obligationStatementColumns(*o, 0)
	default:
		return true, nil
	}
	stored, err := rowStatementKey(row)
	if err != nil {
		return false, err
	}
	now, err := freshStatementKey(columns, fresh)
	if err != nil {
		return false, err
	}
	return stored == now, nil
}

// composeRefusalIsNotSame lets a refusal to compose the message now read as a statement that is not
// the same, and passes any other error on.
func composeRefusalIsNotSame(err error) error {
	if refusal := new(Refusal); errors.As(err, refusal) {
		return nil
	}
	return err
}

func (c *Channel) loadSettings(ctx context.Context, task string) (*delivery.TaskSettings, error) {
	if c.SettingsLoader != nil {
		return c.SettingsLoader(ctx, task)
	}
	return delivery.AuthorizedSettings(ctx, c.Store, task, nil)
}

func sameSettings24(first, second *delivery.TaskSettings) bool {
	if first == nil || second == nil {
		return first == second
	}
	// The bound role is part of what a send reads from a record: a re-read that lands on a different
	// binding must cancel the claim rather than let the transport run on the pair the first read
	// confirmed (the auto-compaction limit is resolved from that role's pair).
	return first.SettingsFreeResume == second.SettingsFreeResume && first.BoundRole == second.BoundRole && pyjson.Dumps(first.Data, pyjson.Options{SortKeys: true}) == pyjson.Dumps(second.Data, pyjson.Options{SortKeys: true})
}

func optionalNumber(n sql.NullInt64) any {
	if n.Valid {
		return n.Int64
	}
	return nil
}

func nullableIntRepr(n sql.NullInt64) string {
	if !n.Valid {
		return "None"
	}
	return fmt.Sprint(n.Int64)
}

func nullableStringRepr(s sql.NullString) string {
	if !s.Valid {
		return "None"
	}
	return pyvalue.StrRepr(s.String)
}
