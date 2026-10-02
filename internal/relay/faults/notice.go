// Package faults carries the faultnotice.py deliverer without owning a daemon loop.
package faults

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const NoticePage = 20
const NoticeParkedHold = "superseded_by_report"
const noticeUnaddressedHold = "hierarchy_unresolved"

// NoticeChannel is the supervisor channel boundary. Resolve must read on ctx's
// store transaction and never observe a host. Attempt/Recover own the transport
// start fence, lifecycle, pacing, and uncertain-send recovery (todo 24).
// Measure records lifecycle.observe through lifecycle.record, not just a boolean.
type NoticeChannel interface {
	Resolve(context.Context, string) (map[string]any, error)
	StageNotice(context.Context, map[string]any) (map[string]any, error)
	Attempt(context.Context, string, float64, string) error
	Recover(context.Context, string, float64) error
	Measure(context.Context, string) error
}

// NoticeDeliverer is a bounded pass; cursors intentionally live only in memory.
// Todo 29 calls Tick after reports with the remaining shared per-tick cap.
type NoticeDeliverer struct {
	Ledger                       *Ledger
	Channel                      NoticeChannel
	Owner                        string
	pendingAfter, uncertainAfter string
	// Test-only crash seam at the same boundary as NoticeDeliverer._return.
	beforeReturn func(context.Context) error
}

type NoticeAnswer struct {
	Delivered int         `json:"delivered"`
	Returned  int         `json:"returned"`
	Measured  int         `json:"measured"`
	Waiting   [][2]string `json:"waiting"`
}

func (d *NoticeDeliverer) Tick(ctx context.Context, now float64, limit int) (NoticeAnswer, error) {
	answer := NoticeAnswer{Waiting: [][2]string{}}
	if err := d.reconcile(ctx, &answer, now); err != nil {
		return answer, err
	}
	if limit <= 0 {
		return answer, nil
	}
	ready, err := d.ready(ctx, &answer, now)
	if err != nil || !ready {
		return answer, err
	}
	taken, err := dReserveDeliverable(ctx, d.Ledger, map[string]string{"--owner": d.Owner, "--limit": strconv.Itoa(limit)}, func(ctx context.Context, one map[string]any) string { return d.WaitingFor(ctx, one, now) })
	if err != nil {
		return answer, err
	}
	for _, one := range taken.(map[string]any)["reserved"].([]any) {
		if err = d.deliver(ctx, one.(map[string]any), &answer, now); err != nil {
			return answer, err
		}
	}
	return answer, nil
}

func noticeSent(state string) bool                     { return state == "dispatched" || state == "read" }
func noticeString(m map[string]any, key string) string { s, _ := m[key].(string); return s }
func noticeAt(now float64) string {
	return time.Unix(int64(now), 0).UTC().Format("2006-01-02T15:04:05+00:00")
}

// NoticeError preserves a boundary exception's Python class/reason, rather than
// inventing a successful answer when a channel or host cannot answer.
type NoticeError struct{ Kind, Detail string }

func (e *NoticeError) Error() string { return e.Kind + ": " + e.Detail }

func (d *NoticeDeliverer) message(ctx context.Context, id string) (row, error) {
	return d.Ledger.one(ctx, "SELECT * FROM supervisor_messages WHERE obligation_kind='fault_notification' AND obligation_id=?", id)
}
func (d *NoticeDeliverer) get(ctx context.Context, id string) (row, error) {
	return d.Ledger.one(ctx, "SELECT * FROM supervisor_messages WHERE message_id=?", id)
}
func (d *NoticeDeliverer) mayHaveSent(ctx context.Context, id string) (bool, error) {
	r, err := d.Ledger.one(ctx, "SELECT request_id FROM supervisor_attempts WHERE message_id=? AND "+store.SupervisorAttemptMayHaveGoneSQL("")+" ORDER BY attempt_no DESC LIMIT 1", id)
	return r != nil, err
}
func noticeAddressed(r row, live map[string]any) bool {
	return text(r, "sender_task_id") == live["sender"] && text(r, "recipient_task_id") == live["recipient"] && text(r, "project_key") == live["projectKey"]
}

// WaitingFor is read-only and can be called inside the reservation write.
func (d *NoticeDeliverer) WaitingFor(ctx context.Context, one map[string]any, now float64) string {
	reason, err := d.waitingFor(ctx, one, now)
	if err != nil {
		return "whether it can be carried now could not be read: " + err.Error()
	}
	return reason
}
func (d *NoticeDeliverer) waitingFor(ctx context.Context, one map[string]any, now float64) (string, error) {
	notice, err := d.Ledger.NoticeFacts(ctx, noticeString(one, "notificationId"))
	if err != nil {
		return "", err
	}
	r, err := d.message(ctx, noticeString(one, "notificationId"))
	if err != nil {
		return "", err
	}
	if r != nil {
		if noticeSent(text(r, "state")) {
			return "", nil
		}
		if !store.SupervisorUnsent(text(r, "state")) {
			return "its message " + text(r, "message_id") + " is " + text(r, "state") + ": a send may be under way, and it is settled from that send's answer", nil
		}
	}
	relation := noticeString(notice, "anchor")
	if relation == "" {
		return "no relationship this store holds places fault " + noticeString(one, "faultId") + " under a project, and its scope names no project, so there is no level above to tell", nil
	}
	if unfit := unfitNotice(notice); unfit != "" {
		return "the fault's " + unfit + " is not a value the ledger writes, so no notice can carry it; fault-show has the fault", nil
	}
	live, err := d.Channel.Resolve(ctx, relation)
	if err != nil {
		if _, ok := err.(*NoticeError); ok {
			return err.Error(), nil
		}
		return "", err
	}
	if unfit := unfitNoticeHierarchy(live); unfit != "" {
		return "the " + unfit + " the linkage names is not a plain identifier, so no notice can carry it; fault-show has the fault", nil
	}
	if r != nil && noticeAddressed(r, live) {
		hold := text(r, "hold_reason")
		if hold != "" && hold != NoticeParkedHold && hold != noticeUnaddressedHold {
			return "its message " + text(r, "message_id") + " is held by the supervisor channel: " + hold, nil
		}
		if next, ok := r.Get("next_eligible_at").(float64); ok && next > now {
			because, err := d.because(ctx, text(r, "message_id"))
			return "the supervisor channel rechecks the level above at " + noticeAt(next) + because, err
		}
	}
	query := "SELECT message_id FROM supervisor_messages WHERE recipient_task_id=? AND " + store.SupervisorAheadSQL("")
	args := []any{live["recipient"], now, now}
	if r != nil && r.Get("recipient_task_id") == live["recipient"] {
		query += " AND " + store.SupervisorOlderThanSQL("")
		args = append(args, r.Get("staged_at"), r.Get("staged_at"), r.Get("message_id"))
	}
	ahead, err := d.Ledger.one(ctx, query+" ORDER BY staged_at,message_id LIMIT 1", args...)
	if err != nil {
		return "", err
	}
	if ahead != nil {
		return "an earlier report to " + noticeString(live, "recipient") + " goes first: message " + text(ahead, "message_id"), nil
	}
	return "", nil
}

func (d *NoticeDeliverer) page(ctx context.Context, state string, after *string) ([]any, error) {
	answer, err := dNotifications(ctx, d.Ledger, map[string]string{"--notification-state": state, "--limit": strconv.Itoa(NoticePage), "--after": *after})
	if err != nil {
		return nil, err
	}
	page := answer.(map[string]any)
	*after = ""
	if page["next"] != nil {
		*after = fmt.Sprint(page["next"])
	}
	return page["notifications"].([]any), nil
}
func noticeWait(ctx context.Context, l *Ledger, id, reason string) (bool, error) {
	result, err := l.exec(ctx, "UPDATE fault_notifications SET last_error=?,updated_at=? WHERE notification_id=? AND state='pending' AND last_error IS NOT ?", reason, l.Clock.ISO(), id, reason)
	if err != nil {
		return false, err
	}
	return result == 1, nil
}
func (d *NoticeDeliverer) wait(ctx context.Context, one map[string]any, reason string, answer *NoticeAnswer) error {
	id := noticeString(one, "notificationId")
	changed, err := noticeWait(ctx, d.Ledger, id, reason)
	if changed {
		answer.Waiting = append(answer.Waiting, [2]string{id, reason})
	}
	return err
}
func noticeUnmeasured(contact map[string]any, now float64) bool {
	if len(contact) == 0 {
		return false
	}
	if contact["contactable"] == nil {
		return contact["asked"] != false
	}
	if contact["contactable"] == true {
		return false
	}
	stamp, err := time.Parse(time.RFC3339Nano, noticeString(contact, "observedAt"))
	if err != nil {
		return true
	}
	age := now - float64(stamp.UnixNano())/1e9
	return age > 900 || age < -60
}
func (d *NoticeDeliverer) ready(ctx context.Context, answer *NoticeAnswer, now float64) (bool, error) {
	page, err := d.page(ctx, "pending", &d.pendingAfter)
	if err != nil {
		return false, err
	}
	ready := false
	measured := map[string]bool{}
	unobserved := map[string]string{}
	for _, item := range page {
		one := item.(map[string]any)
		eligibility, _ := one["eligibility"].(map[string]any)
		if eligibility["eligible"] != true {
			parent := noticeString(eligibility, "parentTaskId")
			contact, _ := eligibility["contact"].(map[string]any)
			if parent != "" && !measured[parent] && unobserved[parent] == "" && noticeUnmeasured(contact, now) {
				if err := d.Channel.Measure(ctx, parent); err != nil {
					kind := "error"
					if e, ok := err.(*NoticeError); ok {
						kind = e.Kind
					}
					unobserved[parent] = "its parent " + parent + " could not be observed (" + kind + "), so whether it can be contacted is unknown"
				} else {
					measured[parent] = true
					answer.Measured++
					ready = true
				}
			}
			if reason := unobserved[parent]; reason != "" {
				if err = d.wait(ctx, one, reason, answer); err != nil {
					return false, err
				}
			}
			continue
		}
		if reason := d.WaitingFor(ctx, one, now); reason == "" {
			ready = true
		} else if err = d.wait(ctx, one, reason, answer); err != nil {
			return false, err
		}
	}
	return ready, nil
}
func (d *NoticeDeliverer) settle(ctx context.Context, command, id string, fields map[string]string, answer *NoticeAnswer, delivered bool) error {
	fields["--notification"] = id
	_, err := dSettle(ctx, d.Ledger, command, fields)
	if err != nil {
		// FaultRefused means another writer settled/lapsed the reservation. All other
		// errors propagate: a failed database write must never count as settlement.
		if strings.HasPrefix(err.Error(), "fault_") {
			return nil
		}
		return err
	}
	if delivered {
		answer.Delivered++
	} else {
		answer.Returned++
	}
	return nil
}
func (d *NoticeDeliverer) returnPending(ctx context.Context, id, token, message, why string, answer *NoticeAnswer) error {
	if d.beforeReturn != nil {
		if err := d.beforeReturn(ctx); err != nil {
			return err
		}
	}
	if err := d.settle(ctx, "fault-notification-fail", id, map[string]string{"--token": token, "--error": why}, answer, false); err != nil {
		return err
	}
	if message != "" {
		return d.Ledger.ParkNotice(ctx, message, why)
	}
	return nil
}
func (d *NoticeDeliverer) deliver(ctx context.Context, one map[string]any, answer *NoticeAnswer, now float64) error {
	id, token := noticeString(one, "notificationId"), noticeString(one, "token")
	notice, err := d.Ledger.NoticeFacts(ctx, id)
	if err != nil {
		return err
	}
	staged, err := d.Channel.StageNotice(ctx, notice)
	if err != nil {
		return d.returnPending(ctx, id, token, "", "nothing was staged: "+err.Error(), answer)
	}
	message := noticeString(staged, "messageId")
	r, err := d.get(ctx, message)
	if err != nil {
		return err
	}
	var attemptErr error
	if store.SupervisorUnsent(text(r, "state")) {
		attemptErr = d.Channel.Attempt(ctx, message, now, d.Owner)
	}
	r, err = d.get(ctx, message)
	if err != nil {
		return err
	}
	if noticeSent(text(r, "state")) {
		ref, err := d.ref(ctx, r)
		if err != nil {
			return err
		}
		return d.settle(ctx, "fault-notification-ack", id, map[string]string{"--token": token, "--ref": ref}, answer, true)
	}
	sent, err := d.mayHaveSent(ctx, message)
	if err != nil {
		return err
	}
	if store.SupervisorUnsent(text(r, "state")) && !sent {
		why, err := d.why(ctx, r, attemptErr)
		if err != nil {
			return err
		}
		return d.returnPending(ctx, id, token, message, why, answer)
	}
	return nil
}
func (d *NoticeDeliverer) reconcile(ctx context.Context, answer *NoticeAnswer, now float64) error {
	page, err := d.page(ctx, "uncertain", &d.uncertainAfter)
	if err != nil {
		return err
	}
	for _, item := range page {
		one := item.(map[string]any)
		if one["owner"] != d.Owner {
			continue
		}
		id := noticeString(one, "notificationId")
		r, err := d.message(ctx, id)
		if err != nil {
			return err
		}
		if r != nil && text(r, "state") == "sending" {
			if err = d.Channel.Recover(ctx, text(r, "message_id"), now); err != nil {
				return err
			}
			r, err = d.message(ctx, id)
			if err != nil {
				return err
			}
		}
		delivered := false
		why := ""
		park := ""
		if r == nil {
			why = "no message was ever staged for it, so nothing was sent"
		} else if noticeSent(text(r, "state")) {
			delivered = true
			why, err = d.ref(ctx, r)
		} else if store.SupervisorUnsent(text(r, "state")) {
			var sent bool
			sent, err = d.mayHaveSent(ctx, text(r, "message_id"))
			if err == nil && !sent {
				why, err = d.why(ctx, r, nil)
				park = text(r, "message_id")
			}
		}
		if err != nil {
			return err
		}
		if why == "" {
			continue
		}
		value := "no"
		if delivered {
			value = "yes"
		}
		if err = d.settle(ctx, "fault-notification-reconcile", id, map[string]string{"--delivered": value, "--ref": why}, answer, delivered); err != nil {
			return err
		}
		if park != "" {
			if err = d.Ledger.ParkNotice(ctx, park, why); err != nil {
				return err
			}
		}
	}
	return nil
}
func (d *NoticeDeliverer) ref(ctx context.Context, r row) (string, error) {
	id := text(r, "message_id")
	attempt, err := d.Ledger.one(ctx, "SELECT request_id FROM supervisor_attempts WHERE message_id=? ORDER BY attempt_no DESC LIMIT 1", id)
	ref := "supervisor message " + id + " " + text(r, "state")
	if attempt != nil {
		ref += ", attempt " + text(attempt, "request_id")
	}
	return ref, err
}
func (d *NoticeDeliverer) why(ctx context.Context, r row, failure error) (string, error) {
	parts := []string{"nothing was sent: its message " + text(r, "message_id") + " is " + text(r, "state")}
	if next, ok := r.Get("next_eligible_at").(float64); ok {
		parts = append(parts, "rechecked at "+noticeAt(next))
	}
	if failure != nil {
		parts = append(parts, failure.Error())
	}
	because, err := d.because(ctx, text(r, "message_id"))
	return strings.Join(parts, ", ") + because, err
}
func (d *NoticeDeliverer) because(ctx context.Context, id string) (string, error) {
	r, err := d.Ledger.one(ctx, "SELECT kind,detail FROM journal WHERE subject=? AND kind IN ('supervisor_message_withheld','supervisor_message_deferred','supervisor_message_paced','supervisor_message_superseded') ORDER BY seq DESC LIMIT 1", id)
	if err != nil || r == nil {
		return "", err
	}
	detail := loadsMap(text(r, "detail"))
	reason := detail["reason"]
	if reason == nil || reason == "" {
		reason = detail["detail"]
	}
	if reason == nil || reason == "" {
		reason = detail["refusal"]
	}
	suffix := ""
	if reason != nil && reason != "" {
		suffix = ": " + fmt.Sprint(reason)
	}
	return " (" + text(r, "kind") + suffix + ")", nil
}

// ParkNotice cannot hide a message that may have sent. PARKED_HOLD keeps a
// pending notice out of the channel's oldest-claimable report queue.
func (l *Ledger) ParkNotice(ctx context.Context, id, reason string) error {
	return l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		result, err := l.exec(ctx, "UPDATE supervisor_messages SET hold_reason=?,updated_at=? WHERE message_id=? AND obligation_kind='fault_notification' AND hold_reason IS NULL AND "+store.SupervisorNeverSentSQL(), NoticeParkedHold, l.Clock.ISO(), id)
		if err != nil {
			return err
		}
		if result == 1 {
			return l.noticeJournal(ctx, "supervisor_notice_parked", id, map[string]any{"reason": reason})
		}
		return nil
	})
}
func (l *Ledger) noticeJournal(ctx context.Context, kind, id string, detail map[string]any) error {
	keys := map[string][]string{
		"supervisor_notice_staged":       {"notificationId", "faultId", "recipient"},
		"supervisor_message_readdressed": {"from", "to", "fromRelationship", "toRelationship", "releasedHold", "releasedState", "reason"},
		"supervisor_message_restated":    {"at", "reason"},
		"supervisor_notice_reopened":     {"hold", "reason"},
		"supervisor_notice_parked":       {"reason"},
	}[kind]
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, dumps(key, false)+": "+dumps(detail[key], false))
	}
	_, err := l.exec(ctx, "INSERT INTO journal(at,kind,subject,detail) VALUES(?,?,?,?)", l.Clock.ISO(), kind, id, "{"+strings.Join(parts, ", ")+"}")
	return err
}
