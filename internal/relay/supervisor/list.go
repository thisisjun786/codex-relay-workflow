package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// A recipient's queue read whole, and a backlog of its notifications parked without a turn each
// (CRW-1486/S4, findings 6 and 9). List only reads. Acknowledge parks under the hold the channel
// already parks a superseded message under, with the journal kind it already writes, whose detail
// names AcknowledgeCommand. Staging the same statement again does not release such a park (the
// daemon's staging visit stages every owed event completion again on each pass); what releases it is
// the message's content moving (a new statement of the obligation, re-addressing to another
// recipient) or a send of that message by id. See acknowledgedPark.

// AcknowledgeCommand is the name an acknowledgement's journal rows give as the one who parked them.
const AcknowledgeCommand = "supervisor-acknowledge"

// acknowledgeInformational are the obligation kinds the bulk acknowledgement parks: reports that
// something finished. A block, an omission, an escalation (a design question or a land review the
// run asks its parent), a project block, a lead block, a run that started no node and a session step
// waiting each ask the recipient to act, so the bulk form leaves them; a message named by id may
// still be parked.
var acknowledgeInformational = map[string]bool{
	"completion":            true,
	KindFlowCompletion:      true,
	KindFlowMilestoneReport: true,
	KindLeadReport:          true,
}

// The reasons the acknowledgement gives for a message it does not park.
const (
	AcknowledgeNotFound         = "not_found"         // the store holds no message of that id
	AcknowledgeOtherRecipient   = "other_recipient"   // the message is addressed to another recipient
	AcknowledgeFaultNotice      = "fault_notice"      // a fault notice: its own channel parks it (fault-notification commands)
	AcknowledgeAnswerOwed       = "answer_owed"       // a decision: an answer is owed to it
	AcknowledgeClaimed          = "claimed"           // a send of it is under way, or an attempt of it may have gone
	AcknowledgeNotUnsent        = "not_unsent"        // it was sent or settled already
	AcknowledgeHeld             = "held"              // it already carries a hold
	AcknowledgeNotInformational = "not_informational" // the bulk form parks only the informational kinds
)

// ListRequest is which of a recipient's messages List reads (store.SupervisorListFilter's words:
// State unsent, pending or all; Hold empty, none or a hold word).
type ListRequest struct {
	Recipient, Project, State, Hold string
}

// List is a recipient's messages, in sending order, with their counts by state, hold, project and
// obligation kind. claimable on a row is the channel's own test at now (store.SupervisorClaimableSQL
// for a row already read): unsent, no hold, due.
func (c *Channel) List(ctx context.Context, req ListRequest, now float64) (map[string]any, error) {
	if req.State == "" {
		req.State = store.SupervisorListUnsent
	}
	rows, err := c.Store.ListSupervisorMessages(ctx, store.SupervisorListFilter{Recipient: req.Recipient, Project: req.Project, State: req.State, Hold: req.Hold})
	if err != nil {
		return nil, err
	}
	listed := make([]any, 0, len(rows))
	byState, byHold, byProject, byKind := map[string]any{}, map[string]any{}, map[string]any{}, map[string]any{}
	count := func(into map[string]any, key string) {
		n, _ := into[key].(int)
		into[key] = n + 1
	}
	for _, row := range rows {
		listed = append(listed, map[string]any{
			"messageId":      row.MessageID,
			"relationshipId": row.RelationshipID,
			"projectKey":     nullText(row.ProjectKey),
			"kind":           row.Kind,
			"obligationKind": row.ObligationKind,
			"subject":        row.Subject,
			"state":          row.State,
			"holdReason":     nullText(row.HoldReason),
			"stagedAt":       row.StagedAt,
			"nextEligibleAt": nullNumber(row.NextEligibleAt),
			"claimable":      row.ClaimableAt(now),
		})
		count(byState, row.State)
		count(byHold, textOr(row.HoldReason, store.SupervisorListHoldNone))
		count(byProject, textOr(row.ProjectKey, "none"))
		count(byKind, row.ObligationKind)
	}
	answer := map[string]any{
		"schema":    "supervisor-list/1",
		"recipient": req.Recipient,
		"project":   emptyNull(req.Project),
		"state":     req.State,
		"hold":      emptyNull(req.Hold),
		"rows":      listed,
		"counts":    map[string]any{"total": len(rows), "byState": byState, "byHold": byHold, "byProject": byProject, "byObligationKind": byKind},
	}
	return answer, nil
}

// AcknowledgeRequest is one acknowledgement: the recipient whose messages are parked, the task that
// parks them (written into each journal row), and either the bulk form (AllInformational, narrowed
// to one project and to messages staged before a time) or the messages named by id. At is the
// time written on the rows.
type AcknowledgeRequest struct {
	Recipient, Actor string
	AllInformational bool
	Project          string
	StagedBefore     *time.Time
	Messages         []string
	At               string
}

// Acknowledge parks the messages the request names under store.SupervisorHoldSuperseded, in one
// transaction: an unsent message carrying no hold, nothing of which can have gone, that is not a
// decision and not a fault notice; under the bulk form, also only an informational kind. Every
// other message it considered is reported with the reason it was left. The bulk form considers the
// recipient's messages no send of which has settled; a message named by id is considered whatever
// its state.
func (c *Channel) Acknowledge(ctx context.Context, req AcknowledgeRequest) (map[string]any, error) {
	if req.AllInformational == (len(req.Messages) > 0) {
		return nil, Refusal{"malformed_receipt", "an acknowledgement parks either the recipient's informational notifications or the messages it names, one of the two"}
	}
	parked := []string{}
	skipped := []any{}
	err := c.Store.Compose(ctx, func(tx context.Context, _ *sql.Conn) error {
		parked, skipped = []string{}, []any{}
		skip := func(id, reason string) {
			skipped = append(skipped, map[string]any{"messageId": id, "reason": reason})
		}
		var candidates []store.SupervisorMessagesRow
		if req.AllInformational {
			rows, err := c.Store.ListSupervisorMessages(tx, store.SupervisorListFilter{Recipient: req.Recipient, Project: req.Project, State: store.SupervisorListPending})
			if err != nil {
				return err
			}
			for _, row := range rows {
				if req.StagedBefore != nil && !stagedBefore(row.StagedAt, *req.StagedBefore) {
					continue
				}
				candidates = append(candidates, row)
			}
		} else {
			seen := map[string]bool{}
			for _, id := range req.Messages {
				if seen[id] {
					continue
				}
				seen[id] = true
				row, err := c.Store.SupervisorMessage(tx, id)
				if errors.Is(err, sql.ErrNoRows) {
					skip(id, AcknowledgeNotFound)
					continue
				}
				if err != nil {
					return err
				}
				if row.RecipientTaskID != req.Recipient {
					skip(id, AcknowledgeOtherRecipient)
					continue
				}
				candidates = append(candidates, row)
			}
		}
		var ids []string
		for _, row := range candidates {
			if reason := acknowledgeSkip(row, req.AllInformational); reason != "" {
				skip(row.MessageID, reason)
				continue
			}
			ids = append(ids, row.MessageID)
		}
		if len(ids) == 0 {
			return nil
		}
		detail := pyjson.Dumps(contract.OrderedObject{
			{Key: "detail", Value: "the recipient's acknowledgement parked this message without a turn; staging the same statement again leaves it parked, and a new statement of it, its re-addressing to another recipient or a send of it by id releases the " + store.SupervisorHoldSuperseded + " hold"},
			{Key: "by", Value: AcknowledgeCommand},
			{Key: "actor", Value: req.Actor},
		}, pyjson.Options{})
		moved, err := c.Store.AcknowledgeSupervisorMessages(tx, ids, req.At, detail)
		if err != nil {
			return err
		}
		done := map[string]bool{}
		for _, id := range moved {
			done[id] = true
		}
		for _, id := range ids {
			if done[id] {
				parked = append(parked, id)
			} else {
				// Unsent and unheld, but the guarded update found an attempt that may have sent it.
				skip(id, AcknowledgeClaimed)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"schema":    "supervisor-acknowledge/1",
		"recipient": req.Recipient,
		"actor":     req.Actor,
		"hold":      store.SupervisorHoldSuperseded,
		"parked":    parked,
		"skipped":   skipped,
		"counts":    map[string]any{"parked": len(parked), "skipped": len(skipped)},
		"release":   acknowledgeRelease,
	}, nil
}

// acknowledgeRelease is what the acknowledgement's answer says releases a park.
const acknowledgeRelease = "staging the same statement again leaves a parked message parked: neither the daemon's staging visit, which stages an event's completion again on every pass while its tracking record is not confirmed, nor its sender repeating a direct report releases it. " +
	"It is released when its content moves while it is still owed (a new statement of the obligation: a corrected or resubmitted work report, a direct report sent again with other text), when the hierarchy re-addresses it to another recipient, or when the relay's send command sends it by its message id (--message)"

// acknowledgedPark reports whether the superseded_by_report hold row carries was set by an
// acknowledgement. Every writer of that hold on a message an acknowledgement may park journals one
// supervisor_message_superseded row with it (holdSuperseded, holdObsoleteClaim,
// AcknowledgeSupervisorMessages; the fault-notice park journals its own kind, and a fault notice is
// never acknowledged), so the newest such row of the message is the one that set the hold it carries,
// and an acknowledgement's names AcknowledgeCommand. It reads the journal only.
func (c *Channel) acknowledgedPark(ctx context.Context, row store.SupervisorMessagesRow) (bool, error) {
	if row.HoldReason.String != store.SupervisorHoldSuperseded {
		return false, nil
	}
	var detail string
	err := c.Store.Q(ctx).QueryRowContext(ctx, "SELECT detail FROM journal WHERE kind='supervisor_message_superseded' AND subject=? ORDER BY rowid DESC LIMIT 1", row.MessageID).Scan(&detail)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var parked struct {
		By string `json:"by"`
	}
	if json.Unmarshal([]byte(detail), &parked) != nil {
		return false, nil
	}
	return parked.By == AcknowledgeCommand, nil
}

// acknowledgedKeptReason is a staging answer for a message an acknowledgement parked and that the
// staging restates unchanged.
const acknowledgedKeptReason = "this fact is already staged and its recipient's acknowledgement parked it; staging the same statement again leaves it parked"

// acknowledgeSkip is why row is not parked, or "" when it may be.
func acknowledgeSkip(row store.SupervisorMessagesRow, allInformational bool) string {
	switch {
	case row.ObligationKind == store.SupervisorNoticeObligationKind:
		return AcknowledgeFaultNotice
	case row.Kind == "decision":
		return AcknowledgeAnswerOwed
	case row.State == "sending":
		return AcknowledgeClaimed
	case !row.Unsent():
		return AcknowledgeNotUnsent
	case row.HoldReason.Valid:
		return AcknowledgeHeld
	case allInformational && !acknowledgeInformational[row.ObligationKind]:
		return AcknowledgeNotInformational
	}
	return ""
}

// stagedBefore reports whether a stored staging time is earlier than before. A time the store holds
// in another form is not read as earlier, so it is never parked by the bulk form's time bound.
func stagedBefore(stagedAt string, before time.Time) bool {
	at, err := time.Parse(time.RFC3339Nano, stagedAt)
	return err == nil && at.Before(before)
}

func nullText(value sql.NullString) any {
	if !value.Valid {
		return nil
	}
	return value.String
}

func textOr(value sql.NullString, otherwise string) string {
	if !value.Valid {
		return otherwise
	}
	return value.String
}

func nullNumber(value sql.NullFloat64) any {
	if !value.Valid {
		return nil
	}
	return value.Float64
}

func emptyNull(value string) any {
	if value == "" {
		return nil
	}
	return value
}
