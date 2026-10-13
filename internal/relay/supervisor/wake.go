package supervisor

import (
	"context"
	"database/sql"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// The idle edge of the supervisor channel (CRW-1334, CRW-1314 design section 3).
//
// A supervisor message whose recipient was busy waits out the doubling backoff deferBusy gives it
// (BusyBase 15 s doubling to BusyMax 300 s), and after BusyMaxAttempts busy answers it is held at
// busy_cap; a message whose attempts kept ending before the send is held at attempt_cap. The backoff
// used to be the only thing that ended the wait, and nothing released either hold. The delivery path
// has had the idle edge since CRW-904: while a head waits, the relay keeps a subscription on its
// recipient, and the recipient's idle or notLoaded report makes the head due at once. The supervisor
// channel now takes the same edge from the same daemon pass, without a table of its own:
//
//   - every unsent, unheld message to that recipient that waits out a busy backoff is made due at the
//     edge (next_eligible_at = now), with a supervisor_message_woken journal row;
//   - a busy_cap or attempt_cap hold of an unsent message to that recipient is released and the message
//     made due, with a supervisor_message_released journal row; a verified readback of another message
//     to the same recipient releases them the same way, because the recipient was then demonstrably
//     reading its thread.
//
// Nothing else about the holds changes: the busy count is not reset, so a released message that meets
// the recipient busy past the cap again is held again and waits for the next edge or readback, and the
// holds other passes own (superseded_by_report, hierarchy_unresolved) are never touched here. Nothing is
// new authority either: whether the recipient is busy is still decided by the lifecycle read before any
// transport call, the send budget still paces the send, and the oldest claimable message to a recipient
// still goes first. A supervisor message's recipient task is also its thread, which is what the App
// Server's status report names (the transport is called with the recipient task id as the thread).

// cappedHolds are the holds an edge or a readback of the recipient releases.
var cappedHolds = []string{delivery.BusyCap, delivery.AttemptCap}

// WakeCount is what one edge did to the messages waiting on its recipient: woken were made due before
// their backoff ended, released had their busy_cap or attempt_cap hold lifted.
type WakeCount struct{ Woken, Released int }

// WakeBusyRecipient applies one idle or notLoaded report of thread to the supervisor messages waiting
// on it, in one transaction. A thread with no waiting message, and a message that is already due,
// write nothing.
func (c *Channel) WakeBusyRecipient(ctx context.Context, thread, status string, now float64) (WakeCount, error) {
	at := c.wakeStamp(now)
	var count WakeCount
	err := c.Store.Transaction(ctx, func(tx context.Context, _ *sql.Conn) error {
		count = WakeCount{}
		rows, err := c.Store.All(tx, "SELECT message_id,next_eligible_at FROM supervisor_messages WHERE recipient_task_id=? AND state='deferred_busy' AND hold_reason IS NULL AND next_eligible_at > ? ORDER BY staged_at,message_id", thread, now)
		if err != nil {
			return err
		}
		for _, row := range rows {
			id, previous := row.Text("message_id"), wakeNumber(row.Get("next_eligible_at"))
			result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET next_eligible_at=?,updated_at=? WHERE message_id=? AND state='deferred_busy' AND hold_reason IS NULL AND next_eligible_at=?", now, at, id, previous)
			if err != nil {
				return err
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				if err != nil {
					return err
				}
				continue
			}
			detail := pyjson.Dumps(contract.OrderedObject{{Key: "thread", Value: thread}, {Key: "status", Value: status}, {Key: "previousEligibleAt", Value: previous}, {Key: "eligibleAt", Value: now}}, pyjson.Options{})
			if _, err := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_woken',?,?)", at, id, detail); err != nil {
				return err
			}
			count.Woken++
		}
		released, err := c.releaseCappedHolds(tx, thread, now, at, contract.OrderedObject{{Key: "by", Value: "idle_edge"}, {Key: "status", Value: status}})
		count.Released = released
		return err
	})
	return count, err
}

// releaseCappedHolds lifts the busy_cap and attempt_cap holds of the unsent messages to recipient and
// makes them due at now, journaling one supervisor_message_released row each with cause appended. It
// runs inside the caller's transaction.
func (c *Channel) releaseCappedHolds(tx context.Context, recipient string, now float64, at string, cause contract.OrderedObject) (int, error) {
	rows, err := c.Store.All(tx, "SELECT message_id,hold_reason,state FROM supervisor_messages WHERE recipient_task_id=? AND hold_reason IN (?,?) AND "+store.SupervisorUnsentSQL("")+" ORDER BY staged_at,message_id", recipient, cappedHolds[0], cappedHolds[1])
	if err != nil {
		return 0, err
	}
	released := 0
	for _, row := range rows {
		id, hold, state := row.Text("message_id"), row.Text("hold_reason"), row.Text("state")
		result, err := c.Store.Q(tx).ExecContext(tx, "UPDATE supervisor_messages SET hold_reason=NULL,next_eligible_at=?,updated_at=? WHERE message_id=? AND hold_reason=? AND state=?", now, at, id, hold, state)
		if err != nil {
			return released, err
		}
		if changed, err := result.RowsAffected(); err != nil || changed != 1 {
			if err != nil {
				return released, err
			}
			continue
		}
		detail := append(contract.OrderedObject{{Key: "thread", Value: recipient}, {Key: "holdReason", Value: hold}, {Key: "state", Value: state}, {Key: "eligibleAt", Value: now}}, cause...)
		if _, err := c.Store.Q(tx).ExecContext(tx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,'supervisor_message_released',?,?)", at, id, pyjson.Dumps(detail, pyjson.Options{})); err != nil {
			return released, err
		}
		released++
	}
	return released, nil
}

// IdleWakeRecipients is every recipient a supervisor message waits on at now, one entry per recipient:
// an unsent, unheld message that waits out a busy backoff (whether or not its deadline has passed, as
// the delivery path's own predicate reads a head), or an unsent message held at busy_cap or
// attempt_cap, which only an edge or a readback releases. They are the recipients the daemon keeps a
// subscription on, so their edges reach it at all. Deadline is the earliest unheld message's
// next_eligible_at, and zero for a recipient whose messages are all held: there is no timer to wait
// for, and a hold the daemon cannot take is retried on its own doubling curve.
func (c *Channel) IdleWakeRecipients(ctx context.Context, now float64) ([]delivery.IdleWakeHold, error) {
	rows, err := c.Store.All(ctx, "SELECT recipient_task_id, MIN(CASE WHEN hold_reason IS NULL THEN COALESCE(next_eligible_at, ?) END) AS deadline FROM supervisor_messages"+
		" WHERE (state='deferred_busy' AND hold_reason IS NULL) OR (hold_reason IN (?,?) AND "+store.SupervisorUnsentSQL("")+")"+
		" GROUP BY recipient_task_id ORDER BY recipient_task_id", now, cappedHolds[0], cappedHolds[1])
	if err != nil {
		return nil, err
	}
	var out []delivery.IdleWakeHold
	for _, row := range rows {
		hold := delivery.IdleWakeHold{RecipientTaskID: row.Text("recipient_task_id"), RecipientThreadID: row.Text("recipient_task_id")}
		hold.Deadline = wakeNumber(row.Get("deadline"))
		out = append(out, hold)
	}
	return out, nil
}

// wakeStamp is the journal and updated_at stamp of a wake at now: the channel's injected clock when
// set, as every other write of the channel stamps.
func (c *Channel) wakeStamp(now float64) string {
	if c.clockISO != nil {
		return c.clockISO()
	}
	return delivery.ISOOf(now)
}

// wakeNumber is a REAL column read back as SQLite stored it (a whole number may come back as an
// integer), and zero for NULL.
func wakeNumber(value any) float64 {
	switch v := value.(type) {
	case float64:
		return v
	case int64:
		return float64(v)
	}
	return 0
}
