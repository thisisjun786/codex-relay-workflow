package faults

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/tracking/ref"
)

var noticeIdentifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
var noticeLinearKey = regexp.MustCompile(`^(?:[A-Z][A-Z0-9_]{0,15}-[0-9]{1,9}|[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)
var noticeLink = regexp.MustCompile(`^https://linear\.app/[a-z0-9][a-z0-9-]{0,63}/issue/[A-Z][A-Z0-9_]{0,15}-[0-9]{1,9}(?:/[a-z0-9-]{1,120})?$`)
var noticeIssueURL = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)/issues/([1-9][0-9]{0,8})$`)
var noticeHex = regexp.MustCompile(`^[0-9a-f]{1,64}$`)
var noticeWrite = regexp.MustCompile(`^write:([A-Za-z0-9]+):(uncertain|failed)$`)

// noticeIssueMax bounds an issue key a notice names: a notice packet stays small, so a tracking ref
// longer than the text routing accepts is not named.
const noticeIssueMax = 256

// isNoticeIssue reports whether s is an issue key a notice may name: a Linear key, a UUID, or a
// tracking ref owner/name#N. The ref is read by the network-free leaf internal/tracking/ref, so
// there is one rule for owner/name and the number.
func isNoticeIssue(s string) bool {
	if noticeLinearKey.MatchString(s) {
		return true
	}
	if len(s) > noticeIssueMax {
		return false
	}
	_, err := ref.Parse(s)
	return err == nil
}

// isNoticeLink reports whether s is the link of a published issue a notice may carry: a Linear issue
// URL, or a GitHub one, https://github.com/<owner>/<name>/issues/<N> whose owner, name and number
// the leaf internal/tracking/ref accepts (the link keeps a 9-digit number). The ledger stays offline:
// the leaf imports no network client.
func isNoticeLink(s string) bool {
	if noticeLink.MatchString(s) {
		return true
	}
	m := noticeIssueURL.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	_, err := ref.Parse(m[1] + "/" + m[2] + "#" + m[3])
	return err == nil
}

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
		if isNoticeIssue(anchor.Text("issue_key")) {
			issue = anchor.Text("issue_key")
		}
	} else if project, ok := scope["projectKey"].(string); ok && noticeIdentifier.MatchString(project) {
		where = "project:" + project
		if key, ok := scope["issueKey"].(string); ok && isNoticeIssue(key) {
			issue = key
		}
	}
	published := r.Text("external_ref")
	if isNoticeIssue(published) || isNoticeLink(published) {
		external = published
	}
	raw := r.Text("reason")
	if strings.HasPrefix(raw, "raised:") {
		reason = "raised by a caller (its words are on the notification: fault-notifications)"
	} else if p := noticeWrite.FindStringSubmatch(raw); p != nil {
		reason = "write " + p[1] + " is " + p[2]
	}
	return map[string]any{"notificationId": id, "deliveryKey": "relay-notification:" + id, "faultId": r.Text("fault_id"), "kind": r.Text("kind"), "reason": reason, "cycle": r.Get("cycle"), "state": r.Text("state"), "leaseUntil": r.Get("lease_until"), "attempts": r.Get("attempts"), "product": r.Text("fault_product"), "faultClass": r.Text("fault_class"), "severity": r.Text("severity"), "faultState": r.Text("fault_state"), "externalRef": external, "issuePublished": published != "", "anchor": where, "issueKey": issue}, nil
}

// IsNoticeIssue reports whether s is an issue key a notice may name: TEAM-123, a UUID or a tracking
// ref owner/name#N.
func IsNoticeIssue(s string) bool { return isNoticeIssue(s) }

// UnfitNotice names the first fact of a notice that is not a value the ledger writes, or "" when each
// is. The deliverer asks it before it waits on a notice and the channel asks it before it composes one.
func UnfitNotice(n map[string]any) string {
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

// UnfitNoticeHierarchy names the first of the project, sender and recipient a linkage reading names that
// is not a plain identifier, or "" when each is.
func UnfitNoticeHierarchy(r map[string]any) string {
	for _, key := range []string{"projectKey", "sender", "recipient"} {
		value, ok := r[key].(string)
		if !ok || !noticeIdentifier.MatchString(value) {
			return key
		}
	}
	return ""
}
