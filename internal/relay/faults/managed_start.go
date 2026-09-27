package faults

import (
	"context"
	"fmt"
	"strings"
)

const managedCreationQuery = "SELECT seq, detail FROM journal WHERE subject = ? AND CASE WHEN kind = 'managed_start_observed' AND json_valid(detail) THEN json_extract(detail, '$.stage') = 'creation' END ORDER BY seq DESC LIMIT 1"

func managedAnswerFacts(issue, status string, answer row) (detail, actual, impact string) {
	if answer == nil {
		actual = fmt.Sprintf("the registry stored receipt status %s for this request and keeps a child id only for a receipt it accepts, so what the host answered and whether it created a child for %s are not established", status, issue)
		return fmt.Sprintf("a managed start for %s holds registry receipt status %s: %s", issue, status, actual), actual, fmt.Sprintf("no attached child is working on %s, and any child the host did create is not attached", issue)
	}
	detail = fmt.Sprintf("a managed start for %s recorded creation_%s", issue, status)
	switch status {
	case "identity_unobserved":
		actual = "the host accepted the creation but returned no usable child or standby identity, so the relay could not attach a child"
		return detail + ": " + actual, actual, fmt.Sprintf("a child the host created for %s is not attached to any assignment, and no managed child is working on it", issue)
	case "settings_unverified":
		actual = "the host created a child whose reported settings did not match the request, so the relay refused to attach it"
		return detail + ": " + actual, actual, fmt.Sprintf("a child the host created for %s is not attached to any assignment, and no managed child is working on it", issue)
	}
	child, _ := loadsMap(text(answer, "detail"))["retainedChildTaskId"].(string)
	if status == "unknown" {
		if child != "" {
			actual = fmt.Sprintf("the managed start recorded the host's answer as unknown, naming child %s; whether that child was created is not established, and the relay retained it and did not attach it", child)
			impact = fmt.Sprintf("child %s, if the host created it, is not attached to any assignment, and no managed child is working on %s", child, issue)
		} else {
			actual = fmt.Sprintf("the managed start recorded the host's answer as unknown, so whether the host created a child for %s is not established", issue)
			impact = fmt.Sprintf("no attached child is working on %s, and any child the host did create is not attached", issue)
		}
	} else if child != "" {
		actual = fmt.Sprintf("the host's receipt said %s and named child %s, which the relay retained and did not attach", status, child)
		impact = fmt.Sprintf("child %s is not attached to any assignment, and no managed child is working on %s", child, issue)
	} else {
		actual = fmt.Sprintf("the host's receipt said %s and named no child", status)
		impact = fmt.Sprintf("no child is working on %s", issue)
	}
	return detail + ": " + actual, actual, impact
}

func (sw *Sweeper) managedStillPresent(ctx context.Context, signature map[string]any) (bool, error) {
	rows, err := sw.Store.All(ctx, "SELECT request_id, receipt_status FROM managed_start_requests WHERE issue_key = ? AND state = 'create_armed' AND (receipt_status IS NULL OR receipt_status != 'accepted')", signature["issueKey"])
	if err != nil {
		return false, err
	}
	for _, r := range rows {
		status := text(r, "receipt_status")
		if status == "" {
			answer, err := sw.Store.One(ctx, managedCreationQuery, text(r, "request_id"))
			if err != nil {
				return false, err
			}
			if answer == nil {
				continue
			}
			reason, _ := loadsMap(text(answer, "detail"))["reason"].(string)
			status = strings.TrimPrefix(reason, "creation_")
			if status == reason {
				continue
			}
		}
		if status == signature["receiptStatus"] {
			return true, nil
		}
	}
	return false, nil
}

// ManagedStartFaults collects only answered, still-armed starts. Subset ported for
// todo 22; todo 24 owns the managed-start command and extends this collector.
func (sw *Sweeper) ManagedStartFaults(ctx context.Context, product string, cursor any) ([]Observation, any, error) {
	after, until, err := sw.rotation(ctx, cursor, "SELECT MAX(request_id) FROM managed_start_requests", false)
	if err != nil || until == nil {
		return nil, nil, err
	}
	afterText, _ := after.(string)
	rows, err := sw.Store.All(ctx, "SELECT request_id, issue_key, receipt_status, workspace, revision, updated_at FROM managed_start_requests WHERE state = 'create_armed' AND (receipt_status IS NULL OR receipt_status != 'accepted') AND request_id > ? AND request_id <= ? ORDER BY request_id LIMIT ?", afterText, until, sweepLimit)
	if err != nil {
		return nil, nil, err
	}
	var observations []Observation
	for _, r := range rows {
		status := text(r, "receipt_status")
		var answer row
		if status == "" {
			answer, err = sw.Store.One(ctx, managedCreationQuery, text(r, "request_id"))
			if err != nil {
				return nil, nil, err
			}
			if answer == nil {
				continue
			}
			detail := loadsMap(text(answer, "detail"))
			reason, _ := detail["reason"].(string)
			if !strings.HasPrefix(reason, "creation_") || len(reason) <= len("creation_") {
				continue
			}
			status = strings.TrimPrefix(reason, "creation_")
		}
		ev := []any{evidence("row", "managed_start_requests:"+text(r, "request_id"), map[string]any{"receiptStatus": r.Get("receipt_status"), "requestRevision": r.Get("revision"), "workspace": r.Get("workspace"), "updatedAt": r.Get("updated_at")})}
		if answer != nil {
			detail := loadsMap(text(answer, "detail"))
			ev = append(ev, evidence("row", fmt.Sprintf("journal:%d", integer(answer, "seq")), map[string]any{"kind": "managed_start_observed", "state": detail["state"], "stage": "creation", "reason": detail["reason"], "retainedChildTaskId": detail["retainedChildTaskId"], "standbyRecovery": detail["standbyRecovery"]}))
		}
		issue := text(r, "issue_key")
		detail, actual, impact := managedAnswerFacts(issue, status, answer)
		limits := []any{"the registry keeps only the latest receipt of an armed request"}
		if answer != nil {
			limits = []any{"read from the newest creation answer the managed start journaled; a start that stopped after arming without journaling one is not seen until the same request is retried"}
		}
		ev = append(ev, sw.facts("the host publishes a child for "+issue+" and the relay attaches it", actual, impact, limits, nil))
		observations = append(observations, Observation{Product: product, FaultClass: "managed_start_failed", Severity: Broken, Signature: map[string]any{"issueKey": issue, "receiptStatus": status}, OccurrenceKey: "managed:" + text(r, "request_id") + ":" + status, Scope: map[string]any{"issueKey": issue}, Detail: detail, Evidence: ev})
	}
	p := pageOf(observations, rows, "request_id", after, until)
	return observations, p.cursor, nil
}
