package delivery

import (
	"context"
	"database/sql"
	"os"
	"slices"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dispatch"
)

// ReceiptPullEvidence is the dispatch_evidence of a completion pulled into its parent's own turn.
const ReceiptPullEvidence = "receipt_pull"

// receiptPulledKind is the journal kind of a pull. Its detail carries the pulled message, the bytes the parent read.
const receiptPulledKind = "receipt_pulled"

// The refusals of a receipt pull (CRW-1310).
const (
	PullCallerUnproven  = "pull_caller_unproven"
	PullTurnNotLive     = "pull_turn_not_live"
	PullNotPullable     = "pull_not_pullable"
	PullBehindOlder     = "pull_behind_older_delivery"
	PullPulledElsewhere = "pull_pulled_by_another_turn"
)

// PullRequest is one receipt-pull: the completion, the turn the parent is in, the task the parent
// names and the thread the calling process reports as its own (CODEX_THREAD_ID, "" when unset).
type PullRequest struct {
	Event  string
	Turn   string
	Task   string
	Caller string
}

// Only a current, unheld completion the parent can pull (or whose send is in
// flight) keeps the order. A withheld or stale receipt cannot block its siblings.
const olderWaitingSQL = "SELECT o.event_id FROM deliveries o JOIN events oe ON oe.event_id = o.event_id JOIN relationships orl ON orl.relationship_id = o.relationship_id WHERE o.recipient_task_id = ? AND o.event_id != ? AND o.kind = ? AND o.state IN (?,?,?) AND o.hold_reason IS NULL AND orl.status = 'active' AND orl.superseded_by IS NULL AND oe.stage = 'final' AND oe.suppressed_reason IS NULL AND oe.execution_generation = orl.execution_generation AND (oe.first_seen_at, o.created_at, o.event_id) < (?, ?, ?) ORDER BY oe.first_seen_at, o.created_at, o.event_id"

// PullReceipt pulls a completion the daemon deferred into its parent's own live turn (CRW-1310), so
// the parent reads it in that turn and acknowledges it from there. The caller must be the recipient
// task: its CODEX_THREAD_ID is the recipient, and the recipient is active with the pulled turn in
// progress on the host. The host is read before the store is judged, and the store judgment repeats
// the recipient check against what it reads inside the transaction. No send or turn start happens.
//
// The record is the delivery's own row: state inbox_only, the pulled turn in dispatch_turn_id and
// dispatch_evidence receipt_pull. The daemon's claim takes only queued, deferred_busy and
// withheld_pre_send rows, so a pulled completion is never sent. The message the parent read is kept in
// the receipt_pulled journal row written with the pull, and a repeated pull from the same turn answers
// those stored bytes after revalidating the relationship, generation and stage.
func (d *Service) PullReceipt(ctx context.Context, adapter Adapter, req PullRequest) (Obj, error) {
	if adapter == nil {
		return nil, dispatch.Host("receipt-pull needs the host adapter to read the live turn; no adapter is registered")
	}
	if !eventIDPattern.MatchString(req.Event) {
		return nil, dispatch.Host("event id must be 32 lowercase hex characters")
	}
	if pyvalue.Strip(req.Turn) == "" {
		return nil, dispatch.Host("turn must be a non-empty string")
	}
	if pyvalue.Strip(req.Task) == "" {
		return nil, dispatch.Host("task must be a non-empty string")
	}
	row, err := d.Find(ctx, req.Event)
	if err != nil {
		return nil, err
	}
	if row == nil {
		return nil, refuse(NotClaimable, "no delivery for %s", strconv.Quote(req.Event))
	}
	recipient, thread := row.S("recipient_task_id"), row.S("recipient_thread_id")
	if req.Caller == "" || req.Caller != req.Task || req.Task != recipient || thread != recipient {
		return nil, refuse(PullCallerUnproven, "a pull is made by the recipient itself: the recipient is %s, --task is %s and CODEX_THREAD_ID is %s", strconv.Quote(recipient), strconv.Quote(req.Task), strconv.Quote(req.Caller))
	}
	facts, err := adapter.ReadThread(ctx, thread)
	if err != nil {
		return nil, err
	}
	turn, err := adapter.ReadTurn(ctx, thread, req.Turn)
	if err != nil {
		return nil, err
	}
	if !isText(facts.RuntimeStatus, "active") || turn == nil || TurnStatus(turn) != "inProgress" {
		return nil, refuse(PullTurnNotLive, "turn %s is not the live turn of %s, which the host reports active", strconv.Quote(req.Turn), strconv.Quote(thread))
	}
	if TurnStartedAt(turn) == nil {
		return nil, refuse(PullTurnNotLive, "turn %s has no start time on the host, so an acknowledgement from it could not be verified", strconv.Quote(req.Turn))
	}
	if _, err := AuthorizedSettings(ctx, d.Store, recipient, d.RoleGate); err != nil {
		return nil, err
	}
	var out Obj
	err = d.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		fresh, err := d.Find(ctx, req.Event)
		if err != nil {
			return err
		}
		if fresh == nil {
			return refuse(NotClaimable, "%s is absent", strconv.Quote(req.Event))
		}
		// The host was read before this transaction: the recipient, thread, kind and relationship judged
		// above must still be the row's now.
		if fresh.S("recipient_task_id") != recipient || fresh.S("recipient_thread_id") != thread || fresh.S("kind") != Completion || fresh.S("relationship_id") != row.S("relationship_id") {
			return refuse(PullNotPullable, "%s changed its recipient, kind or relationship while the host was read", strconv.Quote(req.Event))
		}
		if fresh.S("state") == InboxOnly && fresh.S("dispatch_evidence") == ReceiptPullEvidence {
			if fresh.S("dispatch_turn_id") != req.Turn {
				return refuse(PullPulledElsewhere, "%s was pulled into turn %s, not %s", strconv.Quote(req.Event), strconv.Quote(fresh.S("dispatch_turn_id")), strconv.Quote(req.Turn))
			}
			if err := d.revalidatePulled(ctx, fresh); err != nil {
				return err
			}
			message, err := d.pulledMessage(ctx, req.Event)
			if err != nil {
				return err
			}
			out = append(pullAnswer(req, message), F{Key: "_replay", Value: true})
			return nil
		}
		if err := d.judgePull(ctx, fresh); err != nil {
			return err
		}
		message, err := d.PreviewMessage(ctx, req.Event)
		if err != nil {
			return err
		}
		now := d.Clock.ISO()
		changed, err := execSQL(ctx, d.Store, "UPDATE deliveries SET state = ?, dispatch_evidence = ?, dispatch_turn_id = ?, lease_owner = NULL, lease_until = NULL, updated_at = ? WHERE event_id = ? AND state IN (?,?) AND hold_reason IS NULL AND attempt_count = ?", InboxOnly, ReceiptPullEvidence, req.Turn, now, req.Event, Queued, DeferredBusy, fresh.I("attempt_count"))
		if err != nil {
			return err
		}
		if changed != 1 {
			return refuse(PullNotPullable, "%s changed while its pull was judged", strconv.Quote(req.Event))
		}
		if err := d.abandonWake(ctx, req.Event); err != nil {
			return err
		}
		if err := journal(ctx, d.Store, receiptPulledKind, req.Event, Obj{{Key: "turnId", Value: req.Turn}, {Key: "task", Value: req.Task}, {Key: "from", Value: fresh.S("state")}, {Key: "message", Value: message}}, now); err != nil {
			return err
		}
		out = pullAnswer(req, message)
		return nil
	})
	return out, err
}

// relationshipCurrent is what a pulled or pullable completion needs from its relationship and event:
// the event is final, its relationship is active and not superseded, the recipient is that
// relationship's parent, and the event belongs to exactly the generation the relationship is on.
func (d *Service) relationshipCurrent(ctx context.Context, row Row) error {
	eventID := row.S("event_id")
	// Reuse the daemon claim's revision judgment as well as the generation check:
	// a later final receipt can replace this one without opening a new generation.
	if reason, err := d.SupersessionReason(ctx, eventID); err != nil {
		return err
	} else if reason != "" {
		return refuse(PullNotPullable, "%s is superseded (%s)", strconv.Quote(eventID), reason)
	}
	event, err := d.eventRow(ctx, eventID)
	if err != nil {
		return err
	}
	if event == nil {
		return refuse(PullNotPullable, "%s has no event", strconv.Quote(eventID))
	}
	if event.S("stage") != "final" {
		return refuse(PullNotPullable, "%s is staged (%s), not final", strconv.Quote(eventID), strconv.Quote(event.S("stage")))
	}
	relationship, err := LoadRelationship(ctx, d.Store, event.S("relationship_id"))
	if err != nil {
		return err
	}
	if relationship.Status != "active" || relationship.SupersededBy != nil {
		return refuse(PullNotPullable, "the relationship of %s is not active", strconv.Quote(eventID))
	}
	if relationship.Parent.TaskID != row.S("recipient_task_id") {
		return refuse(PullNotPullable, "%s is addressed to %s, which is not the parent of its relationship", strconv.Quote(eventID), strconv.Quote(row.S("recipient_task_id")))
	}
	if event.I("execution_generation") != relationship.Generation {
		return refuse(PullNotPullable, "%s belongs to generation %d, not the current generation %d", strconv.Quote(eventID), event.I("execution_generation"), relationship.Generation)
	}
	return nil
}

// judgePull is what a first pull needs from the store, read inside the pull's transaction: a
// completion still waiting to be sent, not held, under a current final event, with no older delivery
// to the same parent ahead of it.
func (d *Service) judgePull(ctx context.Context, row Row) error {
	eventID, recipient := row.S("event_id"), row.S("recipient_task_id")
	if row.S("kind") != Completion {
		return refuse(PullNotPullable, "%s is a %s; only a completion is pulled into its parent's turn", strconv.Quote(eventID), strconv.Quote(row.S("kind")))
	}
	if !slices.Contains([]string{Queued, DeferredBusy}, row.S("state")) {
		return refuse(PullNotPullable, "%s is %s; only a queued or deferred_busy completion is pulled", strconv.Quote(eventID), strconv.Quote(row.S("state")))
	}
	if row.S("hold_reason") != "" {
		return refuse(PullNotPullable, "%s is held (%s)", strconv.Quote(eventID), row.S("hold_reason"))
	}
	if err := d.relationshipCurrent(ctx, row); err != nil {
		return err
	}
	event, err := d.eventRow(ctx, eventID)
	if err != nil {
		return err
	}
	older, err := all(ctx, d.Store, olderWaitingSQL, recipient, eventID, Completion, Queued, DeferredBusy, Sending, event.S("first_seen_at"), row.S("created_at"), eventID)
	if err != nil {
		return err
	}
	for _, previous := range older {
		reason, err := d.SupersessionReason(ctx, previous.S("event_id"))
		if err != nil {
			return err
		}
		if reason == "" {
			return refuse(PullBehindOlder, "%s waits behind %s, an older delivery to the same parent", strconv.Quote(eventID), strconv.Quote(previous.S("event_id")))
		}
	}
	return nil
}

// revalidatePulled is what a replay needs: the pulled row still stands as the pull left it, not held,
// and its relationship, generation and stage are still current.
func (d *Service) revalidatePulled(ctx context.Context, row Row) error {
	if row.S("kind") != Completion || row.S("hold_reason") != "" {
		return refuse(PullNotPullable, "%s no longer stands as a pulled completion", strconv.Quote(row.S("event_id")))
	}
	return d.relationshipCurrent(ctx, row)
}

// pulledMessage reads the message the parent was given by the pull that stands: the latest
// receipt_pulled journal row of the completion. A missing record is a refusal, never a re-render.
func (d *Service) pulledMessage(ctx context.Context, eventID string) (string, error) {
	row, err := one(ctx, d.Store, "SELECT detail FROM journal WHERE kind = ? AND subject = ? ORDER BY seq DESC LIMIT 1", receiptPulledKind, eventID)
	if err != nil {
		return "", err
	}
	if row == nil {
		return "", refuse(PullNotPullable, "%s has no pulled message on record", strconv.Quote(eventID))
	}
	message := pyjson.Text(loadsObj(row.S("detail")).Get("message"))
	if message == "" {
		return "", refuse(PullNotPullable, "%s has an empty pulled message on record", strconv.Quote(eventID))
	}
	return message, nil
}

func pullAnswer(req PullRequest, message string) Obj {
	return Obj{{Key: "eventId", Value: req.Event}, {Key: "recipientTaskId", Value: req.Task}, {Key: "turnId", Value: req.Turn}, {Key: "state", Value: InboxOnly}, {Key: "message", Value: message}}
}

// cmdReceiptPull is the receipt-pull command. The calling process reports its own thread from
// CODEX_THREAD_ID; the host judges that identity against the recipient (PullReceipt), so the flag
// alone is never the proof. The command needs the host, as the acknowledgement does with --socket.
func cmdReceiptPull(c *cliRun) (any, error) {
	c.args["--caller-thread"] = os.Getenv("CODEX_THREAD_ID")
	return needsHost(c)
}
