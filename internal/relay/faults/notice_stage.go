// subset ported for todo 22; todo 24 owns supervisorchannel and packets.
package faults

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// ComposeNotice ports only the packet shape used by fault notices. evidence is
// the channel's command line for this exact store/program, not a bare PATH name.
func ComposeNotice(notice, live map[string]any, observedAt, evidence string) (map[string]any, error) {
	kind, purpose := "notification", "fault_notice"
	if notice["kind"] == "decision" {
		kind, purpose = "decision", "fault_decision"
	}
	if unfit := unfitNoticeHierarchy(live); unfit != "" {
		return nil, &NoticeError{"malformed_receipt", "the " + unfit + " the linkage names for fault '" + noticeString(notice, "faultId") + "'s notice is not a plain identifier, so no notice carries it; nothing was composed"}
	}
	if unfit := unfitNotice(notice); unfit != "" {
		return nil, &NoticeError{"malformed_receipt", "the fault's " + unfit + " is not a value the ledger writes, so no notice carries it; nothing was composed"}
	}
	if issue := noticeString(notice, "issueKey"); issue != "" && !noticeIssue.MatchString(issue) {
		return nil, &NoticeError{"malformed_receipt", "the issue fault '" + noticeString(notice, "faultId") + "'s notice names is not an issue identifier (TEAM-123 or a UUID); nothing was composed"}
	}
	absent := func(reason, detail string) any { return map[string]any{"absent": reason, "detail": detail} }
	reach := map[string]any{}
	for _, stage := range []string{"transport_accepted", "received", "agreed", "applied", "verified"} {
		state, detail := "unmeasured", "nothing readable answered yet"
		if stage != "transport_accepted" && stage != "received" {
			state = "not_applicable"
			detail = "a report upward is transported and read back, and nothing here records that a supervisor agreed, applied or verified anything; what became of it is read from the Linear record, confirmed"
		}
		reach[stage] = map[string]any{"state": state, "source": nil, "detail": detail}
	}
	var decision any = absent("not_applicable", "no user decision is being asked for")
	var owed any = absent("not_applicable", "this kind owes no answer")
	if kind == "decision" {
		reason := noticeString(notice, "reason")
		if reason == "" {
			reason = "a write about it became uncertain or failed for good"
		}
		decision = "fault " + noticeString(notice, "faultClass") + " (" + noticeString(notice, "product") + ") needs a decision: " + reason
		owed = "user"
	}
	relation := "fault:" + noticeString(notice, "faultId")
	scope := "project " + noticeString(live, "projectKey")
	if issue := noticeString(notice, "issueKey"); issue != "" {
		scope += ", issue " + issue
	}
	state := noticeString(notice, "faultState")
	if state == "withdrawn" {
		state += " (it cleared before anything about it was published)"
	}
	notification := "notification " + noticeString(notice, "kind")
	if reason := noticeString(notice, "reason"); reason != "" {
		notification += ": " + reason
	}
	notification += ", cycle " + fmt.Sprint(notice["cycle"])
	published := "no issue published yet"
	if ref := noticeString(notice, "externalRef"); ref != "" {
		published = "issue " + ref
	} else if notice["issuePublished"] == true {
		published = "an issue is published (its reference is on the fault)"
	}
	basis := strings.Join([]string{"fault " + noticeString(notice, "faultClass") + " (" + noticeString(notice, "product") + ") is " + noticeString(notice, "severity") + ", " + state, notification, published, "fault " + noticeString(notice, "faultId")}, "; ")
	env := map[string]any{"version": "relay-envelope/1", "direction": "parent_to_supervisor", "kind": kind, "purpose": purpose, "messageId": sha256Hex("parent_to_supervisor|" + relation + "|" + purpose + "|" + noticeString(notice, "deliveryKey"))[:32], "relationId": relation, "relationRevision": absent("unknown", "the link revision was not read"), "sender": map[string]any{"role": "parent", "taskId": live["sender"]}, "recipient": map[string]any{"role": "supervisor", "taskId": live["recipient"]}, "subject": notice["deliveryKey"], "scope": scope, "basis": basis, "observedAt": observedAt, "evidence": []any{evidence}, "correlationId": absent("not_applicable", "this message answers nothing earlier"), "replyTo": absent("not_applicable", "no reply is directed at one message"), "answerOwedBy": owed, "decision": decision, "reach": reach}
	return map[string]any{"version": "relay-packet/1", "envelope": env, "issue": notice["issueKey"], "generation": nil, "criteriaDigest": nil, "policy": nil, "callback": nil, "artifact": nil, "evidence": []any{evidence}, "body": nil, "activation": nil}, nil
}

// StageNotice freezes one packet per notification, re-addressing only provably
// unsent rows. resolve must join ctx's read; evidence names this channel's store.
func (l *Ledger) StageNotice(ctx context.Context, notice map[string]any, resolve func(context.Context, string) (map[string]any, error), evidence string) (map[string]any, error) {
	relation := noticeString(notice, "anchor")
	if relation == "" {
		return nil, &NoticeError{"unregistered_scope", "fault '" + noticeString(notice, "faultId") + "' is about no relationship this store holds and its scope names no project, so nothing places it under a project and there is no level above to tell; the notification waits"}
	}
	resolution, err := resolve(ctx, relation)
	if err != nil {
		return nil, err
	}
	packet, err := ComposeNotice(notice, resolution, l.Clock.ISO(), evidence)
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
			return &NoticeError{"relation_owner_drift", "the hierarchy moved while this was being decided: it was read as '" + noticeString(resolution, "sender") + "' reporting to '" + noticeString(resolution, "recipient") + "', and under the write lock it is '" + noticeString(live, "sender") + "' reporting to '" + noticeString(live, "recipient") + "'. Nothing was written; staging again addresses the report to the live supervisor"}
		}
		r, err := l.one(ctx, "SELECT * FROM supervisor_messages WHERE obligation_kind='fault_notification' AND obligation_id=?", notice["notificationId"])
		if err != nil {
			return err
		}
		finish := func(staged bool, reason string, restated bool) error {
			r, err := l.one(ctx, "SELECT * FROM supervisor_messages WHERE message_id=?", id)
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
			result, err := l.exec(ctx, "INSERT OR IGNORE INTO supervisor_messages(message_id,obligation_id,obligation_kind,relationship_id,project_key,purpose,kind,sender_task_id,recipient_task_id,subject,packet,state,attempt_count,next_eligible_at,staged_at,updated_at,event_id,submission_no,reading) VALUES(?,?,'fault_notification',?,?,?,?,?,?,?,?,'queued',0,NULL,?,?,NULL,NULL,NULL)", id, notice["notificationId"], relation, resolution["projectKey"], env["purpose"], env["kind"], resolution["sender"], resolution["recipient"], notice["deliveryKey"], dumps(packet, false), stamp, stamp)
			if err != nil {
				return err
			}
			if result == 1 {
				if err = l.noticeJournal(ctx, "supervisor_notice_staged", id, map[string]any{"notificationId": notice["notificationId"], "faultId": notice["faultId"], "recipient": resolution["recipient"]}); err != nil {
					return err
				}
			}
			return finish(result == 1, "", false)
		}
		id = text(r, "message_id")
		moving := !noticeAddressed(r, live)
		moved := moving || text(r, "relationship_id") != relation
		same := dumps(packet, false) == text(r, "packet")
		if same && !moved && r.Get("hold_reason") == nil {
			return finish(false, "this notification is already staged; one notification is one message", false)
		}
		released := (text(r, "hold_reason") == NoticeParkedHold || text(r, "hold_reason") == noticeUnaddressedHold) && !moving
		bounds := " hold_reason=CASE WHEN hold_reason IN (?,?) THEN NULL ELSE hold_reason END,"
		args := []any{dumps(packet, false), relation, live["sender"], live["recipient"], live["projectKey"]}
		if moving {
			bounds = " state=?,next_eligible_at=NULL,hold_reason=NULL,"
			args = append(args, "queued")
		} else {
			args = append(args, NoticeParkedHold, noticeUnaddressedHold)
		}
		args = append(args, stamp, id, r.Get("packet"), r.Get("relationship_id"), r.Get("sender_task_id"), r.Get("recipient_task_id"))
		result, err := l.exec(ctx, "UPDATE supervisor_messages SET packet=?,relationship_id=?,sender_task_id=?,recipient_task_id=?,project_key=?,"+bounds+" updated_at=? WHERE message_id=? AND state IN ('queued','deferred_busy','withheld_pre_send') AND packet=? AND relationship_id=? AND sender_task_id=? AND recipient_task_id=? AND NOT EXISTS(SELECT 1 FROM supervisor_attempts a WHERE a.message_id=supervisor_messages.message_id AND (a.send_attempted <> 'no' OR a.retry_safe=0))", args...)
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
			if err = l.noticeJournal(ctx, "supervisor_message_readdressed", id, map[string]any{"from": r.Get("recipient_task_id"), "to": live["recipient"], "fromRelationship": r.Get("relationship_id"), "toRelationship": relation, "releasedHold": hold, "releasedState": state, "reason": "the notice was never sent and what it is about or the level above moved, so it goes to the one there now"}); err != nil {
				return err
			}
		}
		if !same {
			if err = l.noticeJournal(ctx, "supervisor_message_restated", id, map[string]any{"at": "staging", "reason": "what the notification says about its fault moved and nothing had been sent"}); err != nil {
				return err
			}
		}
		if released {
			if err = l.noticeJournal(ctx, "supervisor_notice_reopened", id, map[string]any{"hold": r.Get("hold_reason"), "reason": "its notification is reserved again"}); err != nil {
				return err
			}
		}
		return finish(false, "", true)
	})
	return answer, err
}
