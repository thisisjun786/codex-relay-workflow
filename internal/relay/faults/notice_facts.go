package faults

import (
	"context"
	"regexp"
	"slices"
	"strings"
)

var noticeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var noticeIssue = regexp.MustCompile(`^(?:[A-Z][A-Z0-9_]{0,15}-[0-9]{1,9}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var noticeLink = regexp.MustCompile(`^https://linear\.app/[a-z0-9][a-z0-9-]{0,63}/issue/[A-Z][A-Z0-9_]{0,15}-[0-9]{1,9}(?:/[a-z0-9-]{1,120})?$`)
var noticeHex = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
var noticeWrite = regexp.MustCompile(`^write:([A-Za-z0-9]+):(uncertain|failed)$`)

// NoticeFacts is faults.notice_facts. Free-text evidence never enters a packet.
func (l *Ledger) NoticeFacts(ctx context.Context, id string) (map[string]any, error) {
	r, err := l.one(ctx, "SELECT n.*,f.product AS fault_product,f.fault_class,f.severity,f.state AS fault_state,f.external_ref,f.signature,f.scope FROM fault_notifications n JOIN fault_ledger f ON f.fault_id=n.fault_id WHERE n.notification_id=?", id)
	if err != nil || r == nil {
		return nil, err
	}
	anchor, err := dAnchor(ctx, l, r)
	if err != nil {
		return nil, err
	}
	var where, issue, external, reason any
	scope := loadsMap(r.Text("scope"))
	if anchor != nil {
		where = anchor.Text("relationship_id")
		if noticeIssue.MatchString(anchor.Text("issue_key")) {
			issue = anchor.Text("issue_key")
		}
	} else if project, ok := scope["projectKey"].(string); ok && noticeIdentifier.MatchString(project) {
		where = "project:" + project
		if key, ok := scope["issueKey"].(string); ok && noticeIssue.MatchString(key) {
			issue = key
		}
	}
	ref := r.Text("external_ref")
	if noticeIssue.MatchString(ref) || noticeLink.MatchString(ref) {
		external = ref
	}
	raw := r.Text("reason")
	if strings.HasPrefix(raw, "raised:") {
		reason = "raised by a caller (its words are on the notification: fault-notifications)"
	} else if p := noticeWrite.FindStringSubmatch(raw); p != nil {
		reason = "write " + p[1] + " is " + p[2]
	}
	return map[string]any{"notificationId": id, "deliveryKey": "relay-notification:" + id, "faultId": r.Text("fault_id"), "kind": r.Text("kind"), "reason": reason, "cycle": r.Get("cycle"), "state": r.Text("state"), "leaseUntil": r.Get("lease_until"), "attempts": r.Get("attempts"), "product": r.Text("fault_product"), "faultClass": r.Text("fault_class"), "severity": r.Text("severity"), "faultState": r.Text("fault_state"), "externalRef": external, "issuePublished": ref != "", "anchor": where, "issueKey": issue}, nil
}

func unfitNotice(n map[string]any) string {
	for _, key := range []string{"faultClass", "product", "severity", "faultState", "kind", "faultId"} {
		value, ok := n[key].(string)
		valid := ok
		switch key {
		case "faultClass":
			valid = valid && len(value) <= 128 && productName.MatchString(value)
		case "product":
			valid = valid && productName.MatchString(value)
		case "severity":
			valid = valid && slices.Contains([]string{"notice", "degraded", "broken"}, value)
		case "faultState":
			valid = valid && slices.Contains([]string{"observed", "open", "fix_pending", "resolved", "withdrawn"}, value)
		case "kind":
			valid = valid && slices.Contains([]string{"blocking", "decision", "resolved"}, value)
		case "faultId":
			valid = valid && noticeHex.MatchString(value)
		}
		if !valid {
			return key
		}
	}
	return ""
}
func unfitNoticeHierarchy(r map[string]any) string {
	for _, key := range []string{"projectKey", "sender", "recipient"} {
		value, ok := r[key].(string)
		if !ok || !noticeIdentifier.MatchString(value) {
			return key
		}
	}
	return ""
}
