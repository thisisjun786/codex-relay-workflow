package faults

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/argparse"
)

func dAttention(ctx context.Context, l *Ledger) (any, error) {
	moment := l.Clock.Now()
	unsent := map[string]any{}
	for _, key := range []string{"ready", "awaitingTarget", "held", "awaitingRecord", "scopeKeyContested", "backingOff", "kindUnregistered", "issueOwned", "unclassified", "claimed", "claimedLapsed", "issued", "issuedLapsed", "failed", "uncertain"} {
		unsent[key] = int64(0)
	}
	rows, e := l.Store.All(ctx, "SELECT p.*,f.scope_key,f.product,f.external_ref AS owned_ref FROM fault_publications p JOIN fault_ledger f ON f.fault_id=p.fault_id WHERE p.state='pending' ORDER BY p.rowid LIMIT 1000")
	if e != nil {
		return nil, e
	}
	for _, r := range rows {
		key, e := dAttentionReason(ctx, l, r, moment)
		if e != nil {
			return nil, e
		}
		unsent[key] = unsent[key].(int64) + 1
	}
	queries := map[string]string{"claimed": "SELECT COUNT(*) AS n FROM fault_publications WHERE state='claimed'", "issued": "SELECT COUNT(*) AS n FROM fault_publications WHERE state='issued'", "failed": "SELECT COUNT(*) AS n FROM fault_publications WHERE state='failed'", "uncertain": "SELECT COUNT(*) AS n FROM fault_publications WHERE state='uncertain'", "claimedLapsed": "SELECT COUNT(*) AS n FROM fault_publications WHERE state='claimed' AND lease_until<=?", "issuedLapsed": "SELECT COUNT(*) AS n FROM fault_publications WHERE state='issued' AND (lease_until IS NULL OR lease_until<=?)", "unclassified": "SELECT MAX(COUNT(*)-?,0) AS n FROM fault_publications WHERE state='pending'"}
	for _, key := range []string{"claimed", "claimedLapsed", "issued", "issuedLapsed", "failed", "uncertain", "unclassified"} {
		var r row
		var err error
		switch key {
		case "claimedLapsed", "issuedLapsed":
			r, err = l.one(ctx, queries[key], l.Clock.Now())
		case "unclassified":
			r, err = l.one(ctx, queries[key], len(rows))
		default:
			r, err = l.one(ctx, queries[key])
		}
		if err != nil {
			return nil, err
		}
		unsent[key] = integer(r, "n")
	}
	notices := map[string]any{}
	for _, state := range []string{"pending", "reserved", "uncertain"} {
		r, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_notifications WHERE state=?", state)
		if e != nil {
			return nil, e
		}
		notices[state] = integer(r, "n")
	}
	r, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_notifications WHERE state='reserved' AND (lease_until IS NULL OR lease_until<=?)", l.Clock.Now())
	if e != nil {
		return nil, e
	}
	notices["reservedLapsed"] = integer(r, "n")
	unlinkedRow, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_links l JOIN fault_ledger f ON f.fault_id=l.fault_id LEFT JOIN fault_targets t ON t.scope_key=f.scope_key LEFT JOIN fault_target_projects tp ON tp.scope_key=f.scope_key WHERE NOT (l.state='linked' AND t.scope_key IS NOT NULL AND tp.product IS f.product AND tp.project_ref IS NOT NULL AND l.observed_project_ref IS tp.project_ref AND NOT EXISTS(SELECT 1 FROM fault_ledger o WHERE o.scope_key=f.scope_key AND o.product!=f.product))")
	if e != nil {
		return nil, e
	}
	unlinked := integer(unlinkedRow, "n")
	parts := []string{}
	for _, key := range []string{"ready", "awaitingTarget", "held", "awaitingRecord", "scopeKeyContested", "backingOff", "kindUnregistered", "issueOwned", "unclassified", "claimed", "claimedLapsed", "issued", "issuedLapsed", "failed", "uncertain"} {
		if n := unsent[key].(int64); n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, key))
		}
	}
	var warning any
	doubtful := notices["uncertain"].(int64) + notices["reservedLapsed"].(int64)
	if unlinked > 0 {
		parts = append(parts, fmt.Sprintf("%d issue(s) not in their project", unlinked))
	}
	if doubtful > 0 {
		parts = append(parts, fmt.Sprintf("%d notification(s) uncertain", doubtful))
	}
	total := int64(0)
	for key, value := range unsent {
		if key != "claimedLapsed" && key != "issued" {
			total += value.(int64)
		}
	}
	if total > 0 || unlinked > 0 || doubtful > 0 {
		warning = "fault writes need attention: " + strings.Join(parts, ", ")
	}
	return map[string]any{"unsent": unsent, "unlinked": unlinked, "notifications": notices, "warning": warning}, nil
}
func dRelink(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	limit, e := dBound(ctx, a["--limit"], "--limit", 100, 1000)
	if e != nil {
		return nil, e
	}
	var result map[string]any
	stamp := l.Clock.ISO()
	e = l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		result, err = dRelinkWork(ctx, l, "", limit, stamp)
		return err
	})
	return result, e
}

// dRelinkWork shares target-mode-aware repointing and relinking between target
// changes and explicit continuation. The caller owns the transaction; limit zero
// counts the remaining work without writing anything.
func dRelinkWork(ctx context.Context, l *Ledger, scope string, limit int, stamp string) (map[string]any, error) {
	backfilled, backfillPending, relinked, relinkPending := 0, int64(0), 0, int64(0)
	e := func() error {
		query, base, selected, where := dRepointQuery()
		if scope != "" {
			query += " AND f.scope_key = ?"
			base += " AND f.scope_key = ?"
			selected = append(selected, scope)
			where = append(where, scope)
		}
		rows, e := l.Store.All(ctx, query+" ORDER BY p.rowid LIMIT ?", append(selected, limit)...)
		if e != nil {
			return e
		}
		backfilled = len(rows)
		for _, r := range rows {
			id := text(r, "publication_id")
			if _, e = l.exec(ctx, "UPDATE fault_publications SET tracker_ref=?,updated_at=? WHERE publication_id=?", r.Get("team"), stamp, id); e != nil {
				return e
			}
			if _, e = l.exec(ctx, "UPDATE fault_publication_payloads SET project_ref=?,updated_at=? WHERE publication_id=?", r.Get("project"), stamp, id); e != nil {
				return e
			}
		}
		remaining, e := l.one(ctx, "SELECT COUNT(*) AS n"+base, where...)
		if e != nil {
			return e
		}
		backfillPending = integer(remaining, "n")
		linkQuery, linkArgs := dRelinkQuery(), dRelinkArgs()
		if scope != "" {
			linkQuery += " AND f.scope_key = ?"
			linkArgs = append(linkArgs, scope)
		}
		links, e := l.Store.All(ctx, linkQuery+" ORDER BY f.rowid LIMIT ?", append(linkArgs, limit+1)...)
		if e != nil {
			return e
		}
		if len(links) > limit {
			links = links[:limit]
		}
		for _, r := range links {
			id := text(r, "fault_id")
			project := text(r, "project_ref")
			if project != "" {
				if e = dRelinkOne(ctx, l, id, project, stamp); e != nil {
					return e
				}
			} else if e = dUnlinkOne(ctx, l, id, stamp); e != nil {
				return e
			}
			relinked++
		}
		pending, e := l.one(ctx, "SELECT COUNT(*) AS n FROM ("+linkQuery+")", linkArgs...)
		if e != nil {
			return e
		}
		relinkPending = integer(pending, "n")
		return nil
	}()
	return map[string]any{"relinked": relinked, "relinkPending": relinkPending, "backfilled": backfilled, "backfillPending": backfillPending}, e
}
func dNotice(r row) map[string]any {
	reason := text(r, "reason")
	reason = strings.TrimPrefix(reason, "raised:")
	var reasonValue any
	if reason != "" {
		reasonValue = reason
	}
	return map[string]any{"notificationId": text(r, "notification_id"), "faultId": text(r, "fault_id"), "product": text(r, "product"), "kind": text(r, "kind"), "reason": reasonValue, "cycle": integer(r, "cycle"), "ref": r.Get("ref"), "state": text(r, "state"), "deliveryKey": "relay-notification:" + text(r, "notification_id"), "owner": r.Get("owner"), "attempts": integer(r, "attempts"), "lastError": r.Get("last_error"), "createdAt": text(r, "created_at"), "deliveredAt": r.Get("delivered_at"), "ackRef": r.Get("ack_ref")}
}

// dLapse is _lapse_notifications: lapsed reservations become uncertain, and a pending
// blocking notice of a fault that withdrew is withdrawn with it (_void_withdrawn).
func dLapse(ctx context.Context, l *Ledger) error {
	stamp := l.Clock.ISO()
	if _, e := l.exec(ctx, "UPDATE fault_notifications SET state='uncertain',token=NULL,updated_at=? WHERE state='reserved' AND lease_until<=?", stamp, l.Clock.Now()); e != nil {
		return e
	}
	_, e := l.exec(ctx, "UPDATE fault_notifications SET state=?,updated_at=? WHERE state=? AND kind=? AND fault_id IN (SELECT fault_id FROM fault_ledger WHERE state=?)", Withdrawn, stamp, "pending", blocking, Withdrawn)
	return e
}
func dNotifications(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	limit, e := dBound(ctx, a["--limit"], "--limit", 20, 1000)
	if e != nil {
		return nil, e
	}
	after := int64(0)
	if a["--after"] != "" {
		n := integerArg(ctx, "--after", a["--after"])
		if n == nil {
			return nil, fmt.Errorf("usage: invalid after cursor")
		}
		after, e = argparse.SQLiteInteger(n)
		if e != nil {
			return nil, e
		}
	}
	state := a["--notification-state"]
	if state == "" {
		state = "pending"
	}
	var rows []row
	if l.Store.ReadOnly() {
		// Project the lease/withdrawal normalization the writer persists, without
		// mutating another runtime's store (faults.py notifications, read_only).
		effective := "CASE WHEN n.state = ? AND n.lease_until <= ? THEN ? WHEN n.state = ? AND n.kind = ? AND EXISTS (SELECT 1 FROM fault_ledger f WHERE f.fault_id = n.fault_id AND f.state = ?) THEN ? ELSE n.state END"
		moment := l.Clock.Now()
		params := []any{"reserved", moment, "uncertain", "pending", blocking, Withdrawn, Withdrawn}
		rows, e = l.Store.All(ctx, "SELECT n.rowid AS seq, n.*, "+effective+" AS effective_state FROM fault_notifications n WHERE ("+effective+") = ? AND n.rowid > ? ORDER BY n.rowid LIMIT ?", append(append(append([]any{}, params...), params...), state, after, limit+1)...)
		for _, r := range rows {
			for i := range r {
				if r[i].Name == "state" {
					r[i].Value = r.Get("effective_state")
				}
			}
		}
	} else {
		if e = dLapse(ctx, l); e != nil {
			return nil, e
		}
		rows, e = l.Store.All(ctx, "SELECT rowid AS seq,* FROM fault_notifications WHERE state=? AND rowid>? ORDER BY rowid LIMIT ?", state, after, limit+1)
	}
	if e != nil {
		return nil, e
	}
	var next any
	if len(rows) > limit {
		next = integer(rows[limit-1], "seq")
		rows = rows[:limit]
	}
	entries := []any{}
	for _, r := range rows {
		entry := dNotice(r)
		if state == "pending" {
			eligibility, e := dEligibility(ctx, l, text(r, "fault_id"), l.Clock.Now())
			if e != nil {
				return nil, e
			}
			entry["eligibility"] = eligibility
		}
		entries = append(entries, entry)
	}
	return map[string]any{"notifications": entries, "next": next}, nil
}
func dRaise(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	reason := a["--reason"]
	if strings.TrimSpace(reason) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: a raised notification names its reason")
	}
	id := a["--fault"]
	var notice string
	e := l.Store.Transaction(ctx, func(ctx context.Context, _ *sql.Conn) error {
		r, e := l.one(ctx, "SELECT product,cycle FROM fault_ledger WHERE fault_id=?", id)
		if e != nil {
			return e
		}
		if r == nil {
			return fmt.Errorf("fault_unknown: no fault '%s'", id)
		}
		notice = sha256Hex(id + "|decision|raised:" + reason)[:idWidth]
		_, e = l.exec(ctx, "INSERT INTO fault_notifications(notification_id,fault_id,product,kind,reason,cycle,ref,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?) ON CONFLICT(notification_id) DO UPDATE SET state=excluded.state,last_error=NULL,updated_at=excluded.updated_at WHERE fault_notifications.state='withdrawn'", notice, id, text(r, "product"), "decision", "raised:"+reason, integer(r, "cycle"), nilIfEmpty(a["--ref"]), "pending", l.Clock.ISO(), l.Clock.ISO())
		return e
	})
	return map[string]any{"notificationId": notice, "faultId": id, "kind": "decision", "reason": reason}, e
}
func dToken(ctx context.Context) (string, error) {
	b := make([]byte, 8)
	reader := io.Reader(rand.Reader)
	if inputs, ok := ctx.Value(f1InputsKey{}).(f1Inputs); ok && inputs.entropy != nil {
		reader = inputs.entropy
	}
	if _, e := io.ReadFull(reader, b); e != nil {
		return "", e
	}
	return hex.EncodeToString(b), nil
}
func dReserve(ctx context.Context, l *Ledger, a map[string]string) (any, error) {
	return dReserveDeliverable(ctx, l, a, nil)
}

func dReserveDeliverable(ctx context.Context, l *Ledger, a map[string]string, deliverable func(context.Context, map[string]any) string) (any, error) {
	var answer any
	err := l.Store.Compose(ctx, func(ctx context.Context, _ *sql.Conn) error {
		var err error
		answer, err = dReserveRows(ctx, l, a, deliverable)
		return err
	})
	return answer, err
}

func dReserveRows(ctx context.Context, l *Ledger, a map[string]string, deliverable func(context.Context, map[string]any) string) (any, error) {
	owner := a["--owner"]
	if strings.TrimSpace(owner) == "" {
		return nil, fmt.Errorf("fault_observation_malformed: a reservation has an owner")
	}
	if owner == "relay-daemon" && deliverable == nil {
		return nil, fmt.Errorf("fault_observation_malformed: owner 'relay-daemon' is the relay daemon's notification deliverer's: its reservations are settled from the supervisor channel's records, so a reserver with a transport of its own names itself")
	}
	limit, e := dBound(ctx, a["--limit"], "--limit", 20, 1000)
	if e != nil {
		return nil, e
	}
	if e = dLapse(ctx, l); e != nil {
		return nil, e
	}
	rows, e := l.Store.All(ctx, "SELECT * FROM (SELECT n.*,n.rowid AS seq,ROW_NUMBER() OVER (PARTITION BY n.product ORDER BY COALESCE(n.examined_seq,0),n.rowid) AS turn FROM fault_notifications n WHERE n.state='pending' AND (SELECT COUNT(*) FROM fault_budget_uses u WHERE u.product=n.product AND u.kind='notification' AND u.used_ts>? - COALESCE((SELECT window_seconds FROM fault_limits WHERE product=n.product AND kind='notification'),3600)) < COALESCE((SELECT max_count FROM fault_limits WHERE product=n.product AND kind='notification'),10)) ORDER BY turn,COALESCE(examined_seq,0),seq LIMIT ?", l.Clock.Now(), limit*4)
	if e != nil {
		return nil, e
	}
	reserved := []any{}
	withheld, held, waiting := 0, 0, 0
	for _, r := range rows {
		if len(reserved) >= limit {
			break
		}
		id := text(r, "notification_id")
		sequence, e := l.one(ctx, "SELECT COALESCE(MAX(examined_seq),0)+1 AS n FROM fault_notifications")
		if e != nil {
			return nil, e
		}
		if _, e = l.exec(ctx, "UPDATE fault_notifications SET examined_seq=? WHERE notification_id=?", integer(sequence, "n"), id); e != nil {
			return nil, e
		}
		eligibility, e := dEligibility(ctx, l, text(r, "fault_id"), l.Clock.Now())
		if e != nil {
			return nil, e
		}
		if eligibility["eligible"] != true {
			withheld++
			continue
		}
		if deliverable != nil {
			if reason := deliverable(ctx, dNotice(r)); reason != "" {
				if _, e = noticeWait(ctx, l, id, reason); e != nil {
					return nil, e
				}
				waiting++
				continue
			}
		}
		product := text(r, "product")
		budget, e := l.one(ctx, "SELECT max_count,window_seconds FROM fault_limits WHERE product=? AND kind='notification'", product)
		if e != nil {
			return nil, e
		}
		maximum := int64(10)
		window := 3600.0
		if budget != nil {
			maximum = integer(budget, "max_count")
			window = budget.Get("window_seconds").(float64)
		}
		spent, e := l.one(ctx, "SELECT COUNT(*) AS n FROM fault_budget_uses WHERE product=? AND kind='notification' AND used_ts>?", product, l.Clock.Now()-window)
		if e != nil {
			return nil, e
		}
		if integer(spent, "n") >= maximum {
			held++
			continue
		}
		attempt := integer(r, "attempts") + 1
		token, e := dToken(ctx)
		if e != nil {
			return nil, e
		}
		if _, e = l.exec(ctx, "INSERT INTO fault_budget_uses(product,kind,ref,used_at,used_ts) VALUES(?,?,?,?,?)", product, "notification", fmt.Sprintf("%s:%d", id, attempt), l.Clock.ISO(), l.Clock.Now()); e != nil {
			return nil, e
		}
		if _, e = l.exec(ctx, "UPDATE fault_notifications SET state='reserved',token=?,owner=?,lease_until=?,attempts=?,updated_at=? WHERE notification_id=?", token, owner, l.Clock.Now()+300, attempt, l.Clock.ISO(), id); e != nil {
			return nil, e
		}
		fresh, e := l.one(ctx, "SELECT * FROM fault_notifications WHERE notification_id=?", id)
		if e != nil {
			return nil, e
		}
		entry := dNotice(fresh)
		entry["token"] = token
		reserved = append(reserved, entry)
	}
	return map[string]any{"reserved": reserved, "withheld": withheld, "held": held, "waiting": waiting}, nil
}
func dSettle(ctx context.Context, l *Ledger, name string, a map[string]string) (any, error) {
	id := a["--notification"]
	if e := dLapse(ctx, l); e != nil {
		return nil, e
	}
	r, e := l.one(ctx, "SELECT * FROM fault_notifications WHERE notification_id=?", id)
	if e != nil {
		return nil, e
	}
	if r == nil {
		return nil, fmt.Errorf("fault_unknown: no notification %s", id)
	}
	reconcile := name == "fault-notification-reconcile"
	if reconcile {
		if text(r, "state") != "uncertain" {
			return nil, fmt.Errorf("fault_state_conflict: only an uncertain notification is reconciled; this one is %s", text(r, "state"))
		}
	} else if text(r, "state") != "reserved" || text(r, "token") != a["--token"] {
		return nil, fmt.Errorf("fault_claim_stale: this reservation token is not the current one")
	}
	delivered := name == "fault-notification-ack" || reconcile && a["--delivered"] == "yes"
	ref := a["--ref"]
	if name == "fault-notification-fail" {
		ref = a["--error"]
	}
	state := "pending"
	if delivered && reconcile && text(r, "owner") == "relay-daemon" {
		went, e := l.one(ctx, "SELECT message_id FROM supervisor_messages WHERE obligation_kind='fault_notification' AND obligation_id=? AND state IN ('dispatched','read')", id)
		if e != nil {
			return nil, e
		}
		if went == nil {
			return nil, fmt.Errorf("fault_state_conflict: notification %s was reserved by the relay daemon's deliverer, whose transport is the supervisor channel, and the channel has not recorded its message as sent; a supervisor-read readback records an arrival, and the deliverer settles it from that", id)
		}
	}
	if !delivered {
		sent, e := l.one(ctx, "SELECT a.request_id FROM supervisor_messages m JOIN supervisor_attempts a ON a.message_id=m.message_id WHERE m.obligation_kind='fault_notification' AND m.obligation_id=? AND (a.send_attempted <> 'no' OR a.retry_safe=0) LIMIT 1", id)
		if e != nil {
			return nil, e
		}
		if sent != nil {
			return nil, fmt.Errorf("fault_state_conflict: supervisor channel attempt %s may have sent this notification, so it is not settled as not delivered: that send's answer, or a verified readback, settles it", text(sent, "request_id"))
		}
	}
	if delivered {
		state = "delivered"
		_, e = l.exec(ctx, "UPDATE fault_notifications SET state=?,token=NULL,delivered_at=?,ack_ref=?,updated_at=? WHERE notification_id=?", state, l.Clock.ISO(), ref, l.Clock.ISO(), id)
	} else {
		_, e = l.exec(ctx, "DELETE FROM fault_budget_uses WHERE product=? AND kind='notification' AND ref=?", text(r, "product"), fmt.Sprintf("%s:%d", id, integer(r, "attempts")))
		if e == nil {
			if text(r, "kind") == "blocking" {
				fault, readErr := l.one(ctx, "SELECT state FROM fault_ledger WHERE fault_id=?", text(r, "fault_id"))
				if readErr != nil {
					return nil, readErr
				}
				if fault != nil && text(fault, "state") == Withdrawn {
					state = "withdrawn"
				}
			}
			_, e = l.exec(ctx, "UPDATE fault_notifications SET state=?,token=NULL,last_error=?,updated_at=? WHERE notification_id=?", state, ref, l.Clock.ISO(), id)
		}
	}
	if e != nil {
		return nil, e
	}
	r, e = l.one(ctx, "SELECT * FROM fault_notifications WHERE notification_id=?", id)
	if e != nil {
		return nil, e
	}
	return dNotice(r), nil
}
