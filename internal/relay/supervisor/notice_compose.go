package supervisor

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/faults"
)

// composeNotice is the packet shape used by fault notices. evidence is
// the channel's command line for this exact store/program, not a bare PATH name.
func composeNotice(notice, live map[string]any, observedAt, evidence string) (map[string]any, error) {
	kind, purpose := "notification", "fault_notice"
	if notice["kind"] == "decision" {
		kind, purpose = "decision", "fault_decision"
	}
	if unfit := faults.UnfitNoticeHierarchy(live); unfit != "" {
		return nil, &faults.NoticeError{Kind: "malformed_receipt", Detail: "the " + unfit + " the linkage names for fault '" + noticeString(notice, "faultId") + "'s notice is not a plain identifier, so no notice carries it; nothing was composed"}
	}
	if unfit := faults.UnfitNotice(notice); unfit != "" {
		return nil, &faults.NoticeError{Kind: "malformed_receipt", Detail: "the fault's " + unfit + " is not a value the ledger writes, so no notice carries it; nothing was composed"}
	}
	if issue := noticeString(notice, "issueKey"); issue != "" && !faults.IsNoticeIssue(issue) {
		return nil, &faults.NoticeError{Kind: "malformed_receipt", Detail: "the issue fault '" + noticeString(notice, "faultId") + "'s notice names is not an issue identifier (TEAM-123 or a UUID); nothing was composed"}
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
	env := map[string]any{"version": "relay-envelope/1", "direction": "parent_to_supervisor", "kind": kind, "purpose": purpose, "messageId": pyvalue.SHA256Hex("parent_to_supervisor|" + relation + "|" + purpose + "|" + noticeString(notice, "deliveryKey"))[:32], "relationId": relation, "relationRevision": absent("unknown", "the link revision was not read"), "sender": map[string]any{"role": "parent", "taskId": live["sender"]}, "recipient": map[string]any{"role": "supervisor", "taskId": live["recipient"]}, "subject": notice["deliveryKey"], "scope": scope, "basis": basis, "observedAt": observedAt, "evidence": []any{evidence}, "correlationId": absent("not_applicable", "this message answers nothing earlier"), "replyTo": absent("not_applicable", "no reply is directed at one message"), "answerOwedBy": owed, "decision": decision, "reach": reach}
	return map[string]any{"version": "relay-packet/1", "envelope": env, "issue": notice["issueKey"], "generation": nil, "criteriaDigest": nil, "policy": nil, "callback": nil, "artifact": nil, "evidence": []any{evidence}, "body": nil, "activation": nil}, nil
}
