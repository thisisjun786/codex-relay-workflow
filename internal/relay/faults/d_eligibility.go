package faults

import "context"

// dEligibility reads the same relationship and host observation that the Python
// notification path reads. A missing host measurement never authorizes a send.
func dEligibility(ctx context.Context, l *Ledger, id string, moment float64) (map[string]any, error) {
	fault, e := l.one(ctx, "SELECT signature,scope FROM fault_ledger WHERE fault_id=?", id)
	if e != nil {
		return nil, e
	}
	if fault == nil {
		return map[string]any{"eligible": true, "reason": "no relationship whose wishes apply"}, nil
	}
	anchor, e := dAnchor(ctx, l, fault)
	if e != nil {
		return nil, e
	}
	return dAnchorEligibility(ctx, l, anchor, moment)
}

// dAnchor is faults.anchor_relationship, shared by addressing and eligibility.
func dAnchor(ctx context.Context, l *Ledger, fault row) (row, error) {
	var e error
	signature := loadsMap(fault.Text("signature"))
	scope := loadsMap(fault.Text("scope"))
	var anchor row
	if relationship, ok := signature["relationship"].(string); ok && named(relationship) {
		anchor, e = l.one(ctx, "SELECT relationship_id,issue_key,status,parent_task_id FROM relationships WHERE relationship_id=?", relationship)
		if e != nil {
			return nil, e
		}
	}
	for _, location := range []struct {
		values map[string]any
		key    string
	}{{scope, "issueKey"}, {signature, "issueKey"}, {signature, "issue"}, {signature, "subject"}} {
		if anchor != nil {
			break
		}
		issue, ok := location.values[location.key].(string)
		if !ok || !named(issue) {
			continue
		}
		anchor, e = l.one(ctx, "SELECT relationship_id,issue_key,status,parent_task_id FROM relationships WHERE issue_key=? AND superseded_by IS NULL ORDER BY created_at DESC LIMIT 1", issue)
		if e != nil {
			return nil, e
		}
	}
	if anchor == nil {
		return nil, nil
	}
	if project, ok := scope["projectKey"].(string); ok && project != "" {
		placed, e := l.one(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id=?", anchor.Text("relationship_id"))
		if e != nil {
			return nil, e
		}
		if placed != nil && placed.Text("project_key") != project {
			return nil, nil
		}
	}
	return anchor, nil
}

func dAnchorEligibility(ctx context.Context, l *Ledger, anchor row, moment float64) (map[string]any, error) {
	if anchor == nil {
		return map[string]any{"eligible": true, "reason": "no relationship whose wishes apply"}, nil
	}
	about := map[string]any{"relationshipId": anchor.Text("relationship_id"), "parentTaskId": anchor.Text("parent_task_id")}
	status := anchor.Text("status")
	if status == "paused" || status == "cancelled" || status == "archived" {
		about["eligible"] = false
		about["reason"] = "the relationship is " + status
		return about, nil
	}
	contact, e := contactable(ctx, l, anchor.Text("parent_task_id"), moment)
	if e != nil {
		return nil, e
	}
	about["contact"] = contact
	eligible := true
	reason := "reportable"
	if contact["contactable"] == false {
		eligible = false
		reason = "no contact: " + contact["reason"].(string)
	} else if contact["contactable"] == nil && contact["asked"] != false {
		eligible = false
		reason = "contact unmeasured: " + contact["reason"].(string)
	}
	about["eligible"] = eligible
	about["reason"] = reason
	return about, nil
}
