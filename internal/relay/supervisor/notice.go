package supervisor

import (
	"context"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
	"time"
)

// NoticeChannel connects the fault deliverer to the same supervisor claim and
// transport machinery as reports; it never creates a second send engine.
type NoticeChannel struct {
	Channel *Channel
	Ledger  *faults.Ledger
	Host    SendAdapter
}

// The deliverer in faults asks the channel through this interface, so faults never imports this package.
var _ faults.NoticeChannel = NoticeChannel{}

func (n NoticeChannel) Resolve(ctx context.Context, anchor string) (map[string]any, error) {
	r, err := n.Channel.Resolve(ctx, anchor)
	if err != nil {
		return nil, err
	}
	return map[string]any{"sender": r.Sender, "recipient": r.Recipient, "projectKey": r.ProjectKey, "initiativeKey": r.InitiativeKey, "source": r.Source}, nil
}
func (n NoticeChannel) Attempt(ctx context.Context, id string, now float64, owner string) error {
	_, err := n.Channel.attempt(ctx, id, n.Host, now, 0, owner)
	return err
}
func (n NoticeChannel) Recover(ctx context.Context, id string, now float64) error {
	r, err := n.Channel.Get(ctx, id)
	if err != nil {
		return err
	}
	_, err = n.Channel.recoverStranded(ctx, r, now)
	return err
}
func (n NoticeChannel) Measure(ctx context.Context, task string) error {
	return delivery.RecordLifecycle(ctx, n.Channel.Store, n.Ledger.Clock, delivery.Observe(ctx, n.Host, task, nil, true))
}

// A staged notice is not permission to send. Re-derive the reservation under
// the same transaction as the claim and again at the transport-start fence.
func (c *Channel) refreshNotice(ctx context.Context, row store.SupervisorMessagesRow, r Resolution, at string, currentAttempt int64) (bool, error) {
	moment, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return false, err
	}
	now := float64(moment.UnixNano()) / 1e9
	ledger := &faults.Ledger{Store: c.Store, Clock: &delivery.FakeClock{T: now}}
	notice, err := ledger.NoticeFacts(ctx, row.ObligationID)
	if err != nil {
		return false, err
	}
	obsolete := func(detail string) (bool, error) {
		return false, Refusal{"superseded_revision", detail + ". Nothing is sent through this message; it is held as 'superseded_by_report', and staging the project stages what is owed now"}
	}
	if notice == nil {
		return obsolete("notification " + pyvalue.StrRepr(row.ObligationID) + " no longer exists")
	}
	lease, ok := notice["leaseUntil"].(float64)
	if notice["state"] != "reserved" || !ok || lease <= now {
		state := fmt.Sprint(notice["state"])
		if state == "reserved" {
			state = "reserved under a lapsed lease"
		}
		return obsolete("notification " + pyvalue.StrRepr(row.ObligationID) + " is " + state + "; a notice goes out only while its notification is reserved, which is where its eligibility and budget are decided")
	}
	if notice["kind"] == "blocking" && notice["faultState"] == "withdrawn" {
		return obsolete("fault " + pyvalue.StrRepr(notice["faultId"].(string)) + " withdrew - it cleared before anything about it landed - so its blocking notice is no longer a new serious block and does not go up")
	}
	eligible, err := ledger.NotificationEligibility(ctx, notice["faultId"].(string), now)
	if err != nil {
		return false, err
	}
	if eligible["eligible"] != true {
		return obsolete("notification " + pyvalue.StrRepr(row.ObligationID) + " is no longer eligible: " + fmt.Sprint(eligible["reason"]))
	}
	if notice["anchor"] != row.RelationshipID {
		return false, Refusal{"relation_owner_drift", "notification " + pyvalue.StrRepr(row.ObligationID) + " was addressed from " + pyvalue.StrRepr(row.RelationshipID) + " and is about " + fmt.Sprint(notice["anchor"]) + " now"}
	}
	staged := evidence.Dict(evidence.Decode(row.Packet), false)
	observed := pyvalue.Str(evidence.Item(staged["envelope"], "observedAt"))
	live := map[string]any{"sender": r.Sender, "recipient": r.Recipient, "projectKey": r.ProjectKey, "initiativeKey": r.InitiativeKey, "source": r.Source}
	packet, err := composeNotice(notice, live, observed, c.command("fault-show", "--fault", notice["faultId"].(string)))
	if err != nil {
		return false, err
	}
	if pyvalue.ItemEqual(staged, packet) {
		return false, nil
	}
	packet, err = composeNotice(notice, live, at, c.command("fault-show", "--fault", notice["faultId"].(string)))
	if err != nil {
		return false, err
	}
	encoded := pyjson.Dumps(packet, pyjson.Options{})
	result, err := c.Store.Q(ctx).ExecContext(ctx, "UPDATE supervisor_messages SET packet=?,updated_at=? WHERE message_id=? AND "+store.SupervisorRestatableSQL("")+" AND NOT EXISTS(SELECT 1 FROM supervisor_attempts WHERE message_id=? AND attempt_no<>? AND "+store.SupervisorAttemptMayHaveGoneSQL("")+")", encoded, at, row.MessageID, row.MessageID, currentAttempt)
	if err != nil {
		return false, err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if n != 1 {
		return obsolete("the report was sent; the staged packet cannot be changed")
	}
	instant := "claim"
	if row.State == "sending" {
		instant = "transport_start"
	}
	detail := pyjson.Dumps(contract.OrderedObject{{Key: "fromEvent", Value: nil}, {Key: "toEvent", Value: nil}, {Key: "fromSubmission", Value: nil}, {Key: "toSubmission", Value: nil}, {Key: "at", Value: instant}, {Key: "reason", Value: "what notification " + pyvalue.StrRepr(row.ObligationID) + " says about its fault moved after it was staged"}}, pyjson.Options{})
	_, err = c.Store.Q(ctx).ExecContext(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_restated',?,?)", at, row.MessageID, detail)
	return true, err
}
