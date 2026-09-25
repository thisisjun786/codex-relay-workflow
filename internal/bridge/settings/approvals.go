package settings

import (
	"reflect"
	"sort"
)

const approvalLimits = "This bridge answers no approval request: it never grants one, never denies one, and a report message never stands in for one. Measured on codex-cli 0.154.0, the host sends each approval request of a turn to every client subscribed to the thread, replays a pending one to a client that resumes the thread later, and applies the first answer from any of them, an error answer as a denial. An answer from this bridge would therefore decide for the thread's approver, so it records each request and leaves it unanswered; the thread's own client decides it, now or when it next opens the thread, and the turn waits until then. That measurement is of that release, not a reading of the connected server, and how a particular client such as Desktop presents a request it did not ask for is not established here. Delivering a report and servicing the code execution a report may provoke are separate capabilities, and only the first one is claimed."

// Approvals reports the host's approval policy without attempting to set it.
func (c Contract) Approvals(response map[string]any) map[string]any {
	observed := response["approvalPolicy"]
	interactive := observed != "never"
	meaning := "Nothing on this thread can ask for an approval, so delivery and approval cannot be confused here."
	if interactive {
		meaning = "This thread may ask for an approval during the turn. This bridge answers none; the host sends the request to every client subscribed to the thread, the thread's own included, and applies the first answer, so the decision stays with the thread's approver and the turn waits until it is made."
	}
	return map[string]any{"declared": c.approval(), "observed": observed, "reviewer": response["approvalsReviewer"], "transmitted": false, "preservation": "omitted_from_resume", "interactive": interactive, "servicedByThisBridge": false, "onApprovalRequest": "left_for_thread_approver", "meaning": meaning, "limits": approvalLimits}
}

const annotationLimit = "thread/read reports neither sandbox nor approvalPolicy, so no change here means only that the fields in covers were compared and unchanged. Anything in unobserved was not compared at all."

// Annotation compares only fields thread/read actually reports; it never
// changes the status of an already accepted dispatch.
func Annotation(before, thread map[string]any) map[string]any {
	after := map[string]any{}
	changed := map[string]any{}
	covers := []string{}
	unobserved := []string{}
	for _, field := range []string{"model", "reasoningEffort", "cwd"} {
		value := thread[field]
		if value == nil {
			if _, present := before[field]; present {
				unobserved = append(unobserved, field)
			}
			continue
		}
		after[field] = value
		if prior, present := before[field]; present {
			covers = append(covers, field)
			if !reflect.DeepEqual(prior, value) {
				changed[field] = map[string]any{"observed": prior, "afterDispatch": value}
			}
		}
	}
	sort.Strings(covers)
	return map[string]any{"fields": after, "concurrentChange": len(changed) > 0, "changed": changed, "covers": covers, "unobserved": unobserved, "limit": annotationLimit}
}
