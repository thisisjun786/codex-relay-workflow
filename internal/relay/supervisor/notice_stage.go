package supervisor

import (
	"context"
	"database/sql"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// StageNotice freezes one packet per notification, re-addressing only provably
// unsent rows. Resolve joins ctx's read; the evidence line names this channel's store.
func (n NoticeChannel) StageNotice(ctx context.Context, notice map[string]any) (map[string]any, error) {
	faultID, _ := notice["faultId"].(string)
	l, resolve, evidence := n.Ledger, n.Resolve, n.Channel.command("fault-show", "--fault", faultID)
	relation := noticeString(notice, "anchor")
	if relation == "" {
		return nil, &faults.NoticeError{Kind: "unregistered_scope", Detail: "fault '" + noticeString(notice, "faultId") + "' is about no relationship this store holds and its scope names no project, so nothing places it under a project and there is no level above to tell; the notification waits"}
	}
	resolution, err := resolve(ctx, relation)
	if err != nil {
		return nil, err
	}
	packet, err := composeNotice(notice, resolution, l.Clock.ISO(), evidence)
	if err != nil {
		return nil, err
	}
	env := packet["envelope"].(map[string]any)
	id := noticeString(env, "messageId")
	stamp := l.Clock.ISO()
	var answer map[string]any
	err = l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		live, err := resolve(ctx, relation)
		if err != nil {
			return err
		}
		if live["sender"] != resolution["sender"] || live["recipient"] != resolution["recipient"] || live["projectKey"] != resolution["projectKey"] {
			return &faults.NoticeError{Kind: "relation_owner_drift", Detail: "the hierarchy moved while this was being decided: it was read as '" + noticeString(resolution, "sender") + "' reporting to '" + noticeString(resolution, "recipient") + "', and under the write lock it is '" + noticeString(live, "sender") + "' reporting to '" + noticeString(live, "recipient") + "'. Nothing was written; staging again addresses the report to the live supervisor"}
		}
		r, err := l.Store.One(ctx, "SELECT * FROM supervisor_messages WHERE obligation_kind='fault_notification' AND obligation_id=?", notice["notificationId"])
		if err != nil {
			return err
		}
		finish := func(staged bool, reason string, restated bool) error {
			r, err := l.Store.One(ctx, "SELECT * FROM supervisor_messages WHERE message_id=?", id)
			if err != nil {
				return err
			}
			answer = map[string]any{"schema": "relay-supervisor-channel/1", "staged": staged, "messageId": id, "message": r, "recipient": r.Get("recipient_task_id"), "sender": r.Get("sender_task_id")}
			if reason != "" {
				answer["reason"] = reason
			}
			if restated {
				answer["restated"] = true
			}
			return nil
		}
		if r == nil {
			result, err := noticeExec(ctx, l.Store, "INSERT OR IGNORE INTO supervisor_messages(message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,attempt_count,next_eligible_at,staged_at,updated_at,event_id,submission_no,reading) VALUES(?,?,'fault_notification',?,?,?,?,?,?,?,?,'queued',0,NULL,?,?,NULL,NULL,NULL)", id, notice["notificationId"], relation, resolution["projectKey"], env["purpose"], env["kind"], resolution["sender"], resolution["recipient"], notice["deliveryKey"], noticeDumps(packet), stamp, stamp)
			if err != nil {
				return err
			}
			if result == 1 {
				if err = n.journal(ctx, "supervisor_notice_staged", id, map[string]any{"notificationId": notice["notificationId"], "faultId": notice["faultId"], "recipient": resolution["recipient"]}); err != nil {
					return err
				}
			}
			return finish(result == 1, "", false)
		}
		id = r.Text("message_id")
		moving := !faults.NoticeAddressed(r, live)
		moved := moving || r.Text("relationship_id") != relation
		same := noticeDumps(packet) == r.Text("packet")
		if same && !moved && r.Get("hold_reason") == nil {
			return finish(false, "this notification is already staged; one notification is one message", false)
		}
		released := (r.Text("hold_reason") == store.SupervisorHoldSuperseded || r.Text("hold_reason") == store.SupervisorHoldUnaddressed) && !moving
		bounds := " hold_reason=CASE WHEN hold_reason IN (?,?) THEN NULL ELSE hold_reason END,"
		args := []any{noticeDumps(packet), relation, live["sender"], live["recipient"], live["projectKey"]}
		if moving {
			bounds = " state=?,next_eligible_at=NULL,hold_reason=NULL,"
			args = append(args, "queued")
		} else {
			args = append(args, store.SupervisorHoldSuperseded, store.SupervisorHoldUnaddressed)
		}
		args = append(args, stamp, id, r.Get("packet"), r.Get("relationship_id"), r.Get("sender_task_id"), r.Get("recipient_task_id"))
		result, err := noticeExec(ctx, l.Store, "UPDATE supervisor_messages SET packet=?,relationship_id=?,sender_task_id=?,recipient_task_id=?,project_key=?,"+bounds+" updated_at=? WHERE message_id=? AND "+store.SupervisorNeverSentSQL()+" AND packet=? AND relationship_id=? AND sender_task_id=? AND recipient_task_id=?", args...)
		if err != nil {
			return err
		}
		if result != 1 {
			return finish(false, "this notification's message may already have gone, so it is left exactly as it is", false)
		}
		if moved {
			var hold, state any
			if moving {
				hold = r.Get("hold_reason")
				state = r.Get("state")
			}
			if err = n.journal(ctx, "supervisor_message_readdressed", id, map[string]any{"from": r.Get("recipient_task_id"), "to": live["recipient"], "fromRelationship": r.Get("relationship_id"), "toRelationship": relation, "releasedHold": hold, "releasedState": state, "reason": "the notice was never sent and what it is about or the level above moved, so it goes to the one there now"}); err != nil {
				return err
			}
		}
		if !same {
			if err = n.journal(ctx, "supervisor_message_restated", id, map[string]any{"at": "staging", "reason": "what the notification says about its fault moved and nothing had been sent"}); err != nil {
				return err
			}
		}
		if released {
			if err = n.journal(ctx, "supervisor_notice_reopened", id, map[string]any{"hold": r.Get("hold_reason"), "reason": "its notification is reserved again"}); err != nil {
				return err
			}
		}
		return finish(false, "", true)
	})
	return answer, err
}

// Park holds a notice's message out of the channel's oldest-claimable queue under the superseded
// hold, once nothing of it can have gone: it cannot hide a message that may have been sent.
func (n NoticeChannel) Park(ctx context.Context, id, reason string) error {
	l := n.Ledger
	return l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		result, err := noticeExec(ctx, l.Store, "UPDATE supervisor_messages SET hold_reason=?,updated_at=? WHERE message_id=? AND obligation_kind='fault_notification' AND hold_reason IS NULL AND "+store.SupervisorNeverSentSQL(), store.SupervisorHoldSuperseded, l.Clock.ISO(), id)
		if err != nil {
			return err
		}
		if result == 1 {
			return n.journal(ctx, "supervisor_notice_parked", id, map[string]any{"reason": reason})
		}
		return nil
	})
}

// journal writes the row a notice's staging, restating, re-addressing, reopening or parking leaves. Its
// detail is spelled by hand, key by key in the order listed here, the way the rows have always read.
func (n NoticeChannel) journal(ctx context.Context, kind, id string, detail map[string]any) error {
	keys := map[string][]string{
		"supervisor_notice_staged":       {"notificationId", "faultId", "recipient"},
		"supervisor_message_readdressed": {"from", "to", "fromRelationship", "toRelationship", "releasedHold", "releasedState", "reason"},
		"supervisor_message_restated":    {"at", "reason"},
		"supervisor_notice_reopened":     {"hold", "reason"},
		"supervisor_notice_parked":       {"reason"},
	}[kind]
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, noticeDumps(key)+": "+noticeDumps(detail[key]))
	}
	_, err := noticeExec(ctx, n.Ledger.Store, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", n.Ledger.Clock.ISO(), kind, id, "{"+strings.Join(parts, ", ")+"}")
	return err
}

func noticeString(m map[string]any, key string) string { s, _ := m[key].(string); return s }

// noticeDumps is the encoding a notice's packet and journal detail are stored in: sorted keys,
// non-ASCII kept, a byte that is not UTF-8 read as U+FFFD (the options the fault ledger hashes with).
func noticeDumps(value any) string {
	return pyjson.Dumps(value, pyjson.Options{SortKeys: true, Unicode: true, Bytes: pyjson.ReplacedAll})
}

func noticeExec(ctx context.Context, s *store.Store, query string, args ...any) (int64, error) {
	result, err := s.Q(ctx).ExecContext(ctx, query, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
