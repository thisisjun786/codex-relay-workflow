package supervisor

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/delivery"
	mergeevidence "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// RecordWorkReport binds a caller's report to the accepted event, never to caller-supplied identity.
// The write and the delivery resubmission gate share a single transaction.
func RecordWorkReport(ctx context.Context, s *store.Store, clock delivery.Clock, eventID string, input map[string]any) (result map[string]any, err error) {
	defer mergeevidence.RecoverPython(&err)
	event, err := s.One(ctx, "SELECT * FROM events WHERE event_id = ?", eventID)
	if err != nil {
		return nil, err
	}
	if event == nil {
		return nil, reportRefusal("malformed_receipt", fmt.Sprintf("no event %s is recorded, so there is nothing for this report to be about; a report is written against an accepted event, not ahead of one", store.PythonRepr(eventID)))
	}
	outcome := event.Get("outcome").(string)
	status := input["cxc_status"]
	if outcome != "revision_request" {
		if err = checkReportStatus(status, outcome); err != nil {
			return nil, err
		}
	} else if text, ok := status.(string); !ok || reportOutcomes[text] == nil {
		return nil, checkReportStatus(status, outcome)
	}
	values := map[string]any{}
	for _, field := range []struct {
		name string
		max  int
	}{{"repository", 200}, {"summary", 1200}, {"next_action", 1200}, {"cxc_reason", 600}} {
		text, e := reportRequired(input[field.name], field.name)
		if e != nil {
			return nil, e
		}
		text, e = reportLine(text, field.name, field.max)
		if e != nil {
			return nil, e
		}
		values[field.name] = text
	}
	for _, field := range []struct {
		name string
		max  int
	}{{"pr_url", 2000}, {"pr_state", 300}, {"base_ref", 300}, {"base_sha", 300}, {"head_sha", 300}, {"criteria_digest", 300}} {
		if input[field.name] == nil {
			values[field.name] = nil
			continue
		}
		text, valid := input[field.name].(string)
		if !valid {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("%s is a string when it is given at all, not %s", field.name, reportType(input[field.name])))
		}
		text, err = reportLine(text, field.name, field.max)
		if err != nil {
			return nil, err
		}
		values[field.name] = text
	}
	var pr any
	if input["pr_number"] != nil {
		n, valid := reportInteger(input["pr_number"])
		if valid && n < 1 {
			valid = false
		}
		if !valid && positiveReportOverflow(input["pr_number"]) {
			return nil, reportRefusal("malformed_receipt", "a pull request number is outside what the store can hold")
		}
		if !valid {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("a pull request number is a positive integer the store can hold, not a %s outside that range", reportType(input["pr_number"])))
		}
		pr = n
		if values["head_sha"] == nil || values["head_sha"] == "" {
			return nil, reportRefusal("malformed_receipt", "a report naming a pull request names the head commit it is about; without one, a later push silently inherits this report")
		}
	} else {
		var supplied []string
		for _, name := range []string{"pr_url", "pr_state"} {
			if values[name] != nil && values[name] != "" {
				supplied = append(supplied, name)
			}
		}
		if len(supplied) != 0 {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("%s describes a pull request, but none is named. Give the pull request number, or leave these out", strings.Join(supplied, " and ")))
		}
	}
	submission := int64(1)
	if v, exists := input["submission_no"]; exists {
		var valid bool
		submission, valid = reportInteger(v)
		if !valid || submission < 1 {
			return nil, reportRefusal("malformed_receipt", "a submission number is a positive integer the store can hold; it is part of this report identity and is printed in the bytes that get frozen")
		}
	}
	evidence, err := reportEntries(input["evidence"], "evidence")
	if err != nil {
		return nil, err
	}
	unresolved, err := reportEntries(input["unresolved"], "unresolved")
	if err != nil {
		return nil, err
	}
	restore, err := validateReportRestore(input["restore"])
	if err != nil {
		return nil, err
	}
	review, err := validateReportReview(input["review"])
	if err != nil {
		return nil, err
	}
	if outcome == "revision_request" {
		if item, ok := review.(map[string]any); ok && item["kind"] == "PASS" {
			return nil, reportRefusal("disposition_conflict", "a revision request cannot carry a PASS verdict: this event exists because the parent ruled needs_changes, and the message would approve and demand changes at the same time")
		}
		if input["handoff"] != nil {
			return nil, reportRefusal("disposition_conflict", "a revision request carries no merge-readiness handoff: this event exists because the parent ruled needs_changes, and the evidence that a candidate is ready comes from the child on the completion it is about")
		}
	} else if review != nil {
		return nil, reportRefusal("disposition_conflict", fmt.Sprintf("a %s report carries no review: a verdict line belongs to a correction, so this judgment would be stored and never delivered. Put the reviewers findings in the unresolved items, or record the review on the revision request", store.PythonRepr(outcome)))
	}
	if outcome == "ready_for_review" && pr != nil && input["handoff"] == nil {
		return nil, reportRefusal("merge_evidence_required", "this report names a pull request and states nothing about its checks or its review, so nothing here says the candidate is ready to hand over. Record the merge-readiness handoff, or report the work blocked if the review is not finished")
	}
	if input["handoff"] != nil {
		handoff, ok := input["handoff"].(map[string]any)
		if !ok {
			return nil, reportRefusal("malformed_receipt", "a merge-readiness handoff is an object, not a "+reportType(input["handoff"]))
		}
		if pr == nil {
			return nil, reportRefusal("malformed_receipt", "a merge-readiness handoff describes a pull request, but none is named; give the pull request number, or leave the handoff out")
		}
		problems := mergeevidence.HandoffProblems(fmt.Sprint(values["head_sha"]), handoff["reviewCoverage"], reportDefault(handoff["checks"], []any{}), handoff["requiredDeclared"], nil, nil)
		if len(problems) > 0 {
			reason := map[string]string{mergeevidence.Malformed: "malformed_receipt", mergeevidence.ReviewUnstated: "merge_review_incomplete", mergeevidence.ReviewIncomplete: "merge_review_incomplete", mergeevidence.ChecksStale: "merge_currency_stale", mergeevidence.RequiredUndeclared: "merge_evidence_required"}[problems[0].Code]
			return nil, reportRefusal(reason, "this candidate is not ready to hand over: "+strings.Join(mergeevidence.Details(problems), "; "))
		}
		if draft, ok := handoff["isDraft"].(bool); !ok {
			return nil, reportRefusal("malformed_receipt", "the handoff states isDraft as true or false; it is how a reviewer knows the review was actually requested, and leaving it out is not the same as false")
		} else if draft {
			return nil, reportRefusal("merge_evidence_required", "the pull request is still a draft, so the review it reports was never actually requested; mark it ready for review before handing it over")
		}
		verified, ok := handoff["baseVerifiedAt"].(string)
		if !ok || !validReportTimestamp(verified) {
			return nil, reportRefusal("merge_evidence_required", "the handoff states the head it is about but not when its base was verified; the parent restates the base immediately before merging, and a base nobody dated cannot be compared against the one it reads")
		}
		if values["base_sha"] == nil || values["base_sha"] == "" {
			return nil, reportRefusal("merge_evidence_required", "a report carrying a merge-readiness handoff names the base commit it was verified against; without one there is nothing for the pre-merge re-read to disagree with")
		}
		if required, ok := handoff["requiredDeclared"].([]any); ok {
			for _, raw := range required {
				if name, ok := raw.(string); ok {
					if _, err = reportLine(name, "a required check name", 300); err != nil {
						return nil, err
					}
				}
			}
		}
		normalized := normalizedReportHandoff(handoff)
		dispositions, dispositionErr := validateThreadDispositions(normalized["threadDispositions"], normalized["reviewCoverage"])
		if dispositionErr != nil {
			return nil, dispositionErr
		}
		normalized["threadDispositions"] = dispositions
		input["handoff"] = normalized
		if items, ok := normalized["threadDispositions"].([]any); ok {
			accepted := []string{}
			for _, raw := range items {
				entry, ok := raw.(map[string]any)
				if !ok || entry["disposition"] != "accepted" {
					continue
				}
				accepted = append(accepted, fmt.Sprintf("    %s: %s - %s owns it, reopens on %s", entry["threadId"], entry["addressedBy"], entry["followUpOwner"], entry["reopenTrigger"]))
			}
			if len(accepted) > 0 {
				total := len(fmt.Sprintf("  accepted by your decision (%d) - confirm each was yours:", len(accepted))) + 1
				for _, line := range accepted {
					total += len(line) + 1
				}
				purpose := "completion"
				if outcome == "ready_for_review" {
					purpose = "review_ready"
				} else if outcome == "blocked_needs_input" {
					purpose = "blocked"
				}
				room := confirmationsRoom(values["summary"].(string), values["cxc_reason"].(string), values["next_action"].(string), purpose)
				if total > room {
					return nil, reportRefusal("merge_evidence_required", fmt.Sprintf("the acceptance confirmations for this candidate render %d bytes and only %d are left for them once this report's summary, reason and next action have taken theirs. They cannot be shortened, because a dropped confirmation hides a decision the parent is credited with and never made, so a candidate whose confirmations do not fit the message is carrying too many accepted defects to hand over at once. Fix some of them, shorten the decision and follow-up references, or split the change", total, room))
				}
			}
		}
	}
	now := clock.ISO()
	err = s.Transaction(ctx, func(tx context.Context, conn *sql.Conn) (err error) {
		defer mergeevidence.RecoverPython(&err)
		if e := AssertReportResubmission(tx, s, eventID, submission); e != nil {
			return e
		}
		encoded := func(v any) string { return mergeevidence.Dumps(reportStoredValue(v), false, false, true) }
		_, e := conn.ExecContext(tx, "DELETE FROM work_reports WHERE event_id = ? AND submission_no = ?", eventID, submission)
		if e != nil {
			return e
		}
		_, e = conn.ExecContext(tx, `INSERT INTO work_reports (event_id,relationship_id,execution_generation,revision_hash,submission_no,repository,pr_number,pr_url,pr_state,base_ref,base_sha,head_sha,criteria_digest,cxc_status,cxc_reason,contract_version,summary,evidence,unresolved,next_action,review,restore,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, eventID, event.Get("relationship_id"), event.Get("execution_generation"), event.Get("revision_hash"), submission, values["repository"], pr, values["pr_url"], values["pr_state"], values["base_ref"], values["base_sha"], values["head_sha"], values["criteria_digest"], status, values["cxc_reason"], reportContractVersion, values["summary"], encoded(evidence), encoded(unresolved), values["next_action"], nullableJSON(review), encoded(restore), now)
		if e != nil {
			return e
		}
		detail := encoded(map[string]any{"repository": values["repository"], "prNumber": pr, "cxcStatus": status, "headSha": values["head_sha"], "submissionNo": submission})
		if _, e = conn.ExecContext(tx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)", now, "work_report_recorded", eventID, detail); e != nil {
			return e
		}
		if _, e = conn.ExecContext(tx, "DELETE FROM work_report_handoffs WHERE event_id = ? AND submission_no = ?", eventID, submission); e != nil {
			return e
		}
		if h, ok := input["handoff"].(map[string]any); ok {
			h = normalizedReportHandoff(h)
			draft := 0
			if h["isDraft"] == true {
				draft = 1
			}
			_, e = conn.ExecContext(tx, `INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,base_verified_at,required_declared,checks,review_coverage,thread_dispositions,criterion_evidence,limitations,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?)`, eventID, submission, draft, h["baseVerifiedAt"], encoded(h["requiredDeclared"]), encoded(h["checks"]), encoded(h["reviewCoverage"]), encoded(h["threadDispositions"]), encoded(reportDefault(h["criterionEvidence"], []any{})), encoded(reportDefault(h["limitations"], []any{})), now)
			if e != nil {
				return e
			}
			coverage, _ := h["reviewCoverage"].(map[string]any)
			_, e = conn.ExecContext(tx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)", now, "handoff_recorded", eventID, encoded(map[string]any{"submissionNo": submission, "headSha": values["head_sha"], "threadsSeen": coverage["totalCount"], "prNumber": pr}))
			if e != nil {
				return e
			}
		}
		projection, e := delivery.ProjectReportRestoration(tx, s, eventID)
		if e != nil {
			return e
		}
		if projection != nil {
			if _, e = conn.ExecContext(tx, "INSERT INTO journal (at,kind,subject,detail) VALUES (?,?,?,?)", now, "restoration_rendered", eventID, encoded(projection)); e != nil {
				return e
			}
		}
		result, e = ReadWorkReport(tx, s, eventID)
		if e == nil && projection != nil {
			result["restoration"] = projection
		}
		return e
	})
	return result, err
}
func validateThreadDispositions(value, review any) ([]any, error) {
	entries, ok := value.([]any)
	if value == nil {
		entries, ok = []any{}, true
	}
	if !ok {
		return nil, reportRefusal("malformed_receipt", "threadDispositions is a list of entries, not "+reportType(value))
	}
	allowed := map[string]bool{"fixed": true, "accepted": true, "not_applicable": true, "duplicate": true, "already_resolved": true, "disputed": true}
	judged := map[string]map[string]any{}
	for _, raw := range entries {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, reportRefusal("malformed_receipt", "each thread disposition is an object naming its thread, not "+reportRepr(raw))
		}
		id, ok := item["threadId"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, reportRefusal("malformed_receipt", "each thread disposition names the review thread it is about")
		}
		id = strings.TrimSpace(id)
		if judged[id] != nil {
			return nil, reportRefusal("malformed_receipt", "two dispositions name thread "+store.PythonRepr(id)+"; one thread carries one judgment, so the second would silently replace the first")
		}
		disposition, _ := item["disposition"].(string)
		if !allowed[disposition] {
			return nil, reportRefusal("merge_review_incomplete", "thread "+store.PythonRepr(id)+" is recorded as "+reportRepr(item["disposition"])+", which is not a judgment. Use one of: fixed, accepted, not_applicable, duplicate, already_resolved, disputed. Resolving a thread is a button, not a finding anybody ruled on")
		}
		note, err := reportRequired(item["evidence"], "a disposition evidence")
		if err != nil {
			return nil, err
		}
		note, err = reportLine(note, "a disposition evidence", 1<<30)
		if err != nil {
			return nil, err
		}
		addressed, _ := item["addressedBy"].(string)
		owner, ownerSet := item["followUpOwner"].(string)
		trigger, triggerSet := item["reopenTrigger"].(string)
		if disposition == "fixed" && strings.TrimSpace(addressed) == "" {
			return nil, reportRefusal("merge_review_incomplete", "thread "+store.PythonRepr(id)+" is recorded fixed without the commit that fixed it; a per-finding trail is the finding, the commit that addressed it, and the recheck")
		}
		if disposition == "accepted" {
			if strings.TrimSpace(addressed) == "" {
				return nil, reportRefusal("merge_review_incomplete", "thread "+store.PythonRepr(id)+" is recorded accepted without naming the parent decision that accepted it; an acceptance is a judgment somebody made and owns, not a fix and not a cleared thread")
			}
			if strings.TrimSpace(owner) == "" {
				return nil, reportRefusal("merge_review_incomplete", "thread "+store.PythonRepr(id)+" is recorded accepted with no follow-up owner; an acceptance that leaves nobody holding the residue is how a known defect stops being anybody's")
			}
			if strings.TrimSpace(trigger) == "" {
				return nil, reportRefusal("merge_review_incomplete", "thread "+store.PythonRepr(id)+" is recorded accepted with no reopen trigger; without one the acceptance cannot be revisited by anything, which is a waiver rather than a deferral")
			}
			for _, field := range []struct{ name, text string }{{"a follow-up owner", strings.TrimSpace(owner)}, {"a reopen trigger", strings.TrimSpace(trigger)}, {"a thread identifier", id}, {"a disposition addressedBy", strings.TrimSpace(addressed)}} {
				if _, err = reportLine(field.text, field.name, 300); err != nil {
					return nil, err
				}
			}
		} else if ownerSet || triggerSet || item["followUpOwner"] != nil || item["reopenTrigger"] != nil {
			return nil, reportRefusal("malformed_receipt", "thread "+store.PythonRepr(id)+" is recorded "+store.PythonRepr(disposition)+" and carries follow-up fields; a follow-up owner and a reopen trigger belong to an acceptance, which is the disposition that leaves a residue for somebody to own")
		}
		var addressedValue any
		if _, ok := item["addressedBy"].(string); ok {
			addressedValue = strings.TrimSpace(addressed)
		}
		judged[id] = map[string]any{"threadId": id, "disposition": disposition, "evidence": note, "addressedBy": addressedValue, "followUpOwner": nilIfBlank(owner), "reopenTrigger": nilIfBlank(trigger)}
	}
	coverage := mergeevidence.Dict(review, false)
	seen := mergeevidence.Items(coverage["threadsSeen"])
	out := make([]any, 0, len(seen))
	unaccounted := []string{}
	for _, raw := range seen {
		id := mergeevidence.Text(raw)
		if judged[id] == nil {
			unaccounted = append(unaccounted, id)
		} else {
			out = append(out, judged[id])
		}
	}
	if len(unaccounted) > 0 {
		return nil, reportRefusal("merge_review_incomplete", "these review threads were seen and carry no judged disposition: "+mergeevidence.Repr(unaccounted))
	}
	return out, nil
}
func nilIfBlank(value string) any {
	if value == "" {
		return nil
	}
	return strings.TrimSpace(value)
}

func confirmationsRoom(summary, reason, nextAction, purpose string) int {
	return max(0, min(2400, 6000-(1558+len(purpose)-len("completion")+len(summary)+len(reason)+len(nextAction))))
}

func validReportTimestamp(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" {
		return false
	}
	if strings.HasSuffix(value, "Z") {
		value = strings.TrimSuffix(value, "Z") + "+00:00"
	}
	_, err := time.Parse("2006-01-02T15:04:05Z07:00", value)
	if err != nil {
		_, err = time.Parse(time.RFC3339Nano, value)
	}
	return err == nil
}

func normalizedReportHandoff(original map[string]any) map[string]any {
	h := make(map[string]any, len(original))
	for key, value := range original {
		h[key] = value
	}
	if timestamp, ok := h["baseVerifiedAt"].(string); ok && strings.HasSuffix(timestamp, "Z") {
		h["baseVerifiedAt"] = strings.TrimSuffix(timestamp, "Z") + "+00:00"
	}
	if required, ok := h["requiredDeclared"].([]any); ok {
		names := make([]string, 0, len(required))
		for _, raw := range required {
			if name, ok := raw.(string); ok {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		sorted := make([]any, len(names))
		for i, name := range names {
			sorted[i] = name
		}
		h["requiredDeclared"] = sorted
	}
	if items, ok := h["threadDispositions"].([]any); ok {
		normalized := make([]any, len(items))
		for i, item := range items {
			entry, ok := item.(map[string]any)
			if !ok {
				normalized[i] = item
				continue
			}
			copy := make(map[string]any, len(entry)+2)
			for key, value := range entry {
				copy[key] = value
			}
			for _, key := range []string{"followUpOwner", "reopenTrigger"} {
				if _, exists := copy[key]; !exists {
					copy[key] = nil
				}
			}
			normalized[i] = copy
		}
		h["threadDispositions"] = normalized
	}
	return h
}
func reportDefault(v, fallback any) any {
	if v == nil {
		return fallback
	}
	return v
}
func nullableJSON(v any) any {
	if v == nil {
		return nil
	}
	return mergeevidence.Dumps(reportStoredValue(v), false, false, true)
}

// The validators construct these records in Python field order. Go maps do not
// retain that order; restore it before writing json.dumps's default wire format.
func reportStoredValue(value any) any {
	switch v := value.(type) {
	case []any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = reportStoredValue(item)
		}
		return out
	case map[string]any:
		order := ""
		if _, ok := v["check"]; ok {
			order = "check exitCode detail"
		} else if _, ok := v["findings"]; ok {
			order = "kind blockers findings"
		} else if _, ok := v["verdict"]; ok {
			order = "id verdict note anchor"
		} else if _, ok := v["id"]; ok {
			order = "id note"
		} else if _, ok := v["cxcStatus"]; ok {
			order = "repository prNumber cxcStatus headSha submissionNo"
		} else if _, ok := v["restoreSection"]; ok {
			order = "outcome basis criterion detail attempt restoreSection"
		} else if v["basis"] == "relay-report/1" {
			order = "outcome basis criterion detail"
		} else if _, ok := v["skills"]; ok {
			order = "skills"
		}
		keys := strings.Fields(order)
		extra := []string{}
		for key := range v {
			found := false
			for _, ordered := range keys {
				found = found || key == ordered
			}
			if !found {
				extra = append(extra, key)
			}
		}
		sort.Strings(extra)
		out := contract.OrderedObject{}
		for _, key := range append(keys, extra...) {
			if item, ok := v[key]; ok {
				out = append(out, contract.Field{Key: key, Value: reportStoredValue(item)})
			}
		}
		return out
	default:
		return value
	}
}
func positiveReportOverflow(v any) bool {
	switch n := v.(type) {
	case uint64:
		return n > math.MaxInt64
	case json.Number:
		text := string(n)
		if strings.HasPrefix(text, "-") {
			return false
		}
		if parsed, err := strconv.ParseUint(text, 10, 64); err == nil {
			return parsed > math.MaxInt64
		}
		return len(text) > 19 && strings.Trim(text, "0123456789") == ""
	}
	return false
}
func reportInteger(v any) (int64, bool) { return mergeevidence.PyInt(v) }
func reportType(v any) string           { return mergeevidence.TypeName(v) }
func reportLine(text, field string, limit int) (string, error) {
	if !utf8.ValidString(text) {
		return "", reportRefusal("malformed_receipt", field+" contains a character that cannot be encoded as UTF-8, so it cannot be measured against the message budget or sent")
	}
	if strings.ContainsAny(text, "\n\r\v\f\u0085\u2028\u2029") {
		return "", reportRefusal("malformed_receipt", field+" is one line: a line break in it is spliced into the message and adds a line to the protocol rather than wrapping. Put longer detail in the evidence or the unresolved items")
	}
	if len(text) > limit {
		return "", reportRefusal("malformed_receipt", fmt.Sprintf("%s is %d bytes and the limit is %d; a required line cannot be shortened at render time without losing what it exists to say, and a render that fails inside the delivery claim is a delivery that never goes out. Put the detail in the deliverables or the evidence and keep this line to the point", field, len(text), limit))
	}
	return text, nil
}
func reportEntries(v any, field string) ([]any, error) {
	if v == nil {
		return []any{}, nil
	}
	entries, ok := v.([]any)
	if !ok {
		return nil, reportRefusal("malformed_receipt", fmt.Sprintf("%s is a list of entries, not %s", field, reportType(v)))
	}
	out := make([]any, 0, len(entries))
	for _, raw := range entries {
		item, text := raw.(string)
		if text {
			label := "an evidence entry"
			if field == "unresolved" {
				label = "an unresolved entry"
			}
			value, err := reportRequired(item, label)
			if err != nil {
				return nil, err
			}
			value, err = reportLine(value, label, 1<<30)
			if err != nil {
				return nil, err
			}
			out = append(out, value)
			continue
		}
		obj, ok := raw.(map[string]any)
		if field == "evidence" {
			check, valid := obj["check"].(string)
			if !ok || !valid || strings.TrimSpace(check) == "" {
				return nil, reportRefusal("malformed_receipt", fmt.Sprintf("each verification entry is a string or an object naming its check, not %s", reportRepr(raw)))
			}
			check, err := reportLine(strings.TrimSpace(check), "an evidence check", 1<<30)
			if err != nil {
				return nil, err
			}
			detail := ""
			if obj["detail"] != nil {
				var valid bool
				detail, valid = obj["detail"].(string)
				if !valid {
					return nil, reportRefusal("malformed_receipt", fmt.Sprintf("an evidence detail is a line of text when it is given at all, not %s", reportType(obj["detail"])))
				}
			}
			detail, err = reportLine(strings.TrimSpace(detail), "an evidence detail", 1<<30)
			if err != nil {
				return nil, err
			}
			var code any
			if obj["exitCode"] != nil {
				n, valid := reportInteger(obj["exitCode"])
				if !valid {
					return nil, reportRefusal("malformed_receipt", fmt.Sprintf("an exit code is an integer or absent, not %s; evidence a reader cannot interpret is not evidence", reportRepr(obj["exitCode"])))
				}
				if n < -(1<<31) || n > 1<<31 {
					return nil, reportRefusal("malformed_receipt", "an exit code is a number a process could actually have exited with")
				}
				code = n
			}
			var detailValue any
			if detail != "" {
				detailValue = detail
			}
			out = append(out, map[string]any{"check": check, "exitCode": code, "detail": detailValue})
			continue
		}
		id, valid := obj["id"].(string)
		if !ok || !valid || strings.TrimSpace(id) == "" {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("each unresolved entry is a string or an object naming its id, not %s", reportRepr(raw)))
		}
		note := ""
		if obj["note"] != nil {
			var valid bool
			note, valid = obj["note"].(string)
			if !valid {
				return nil, reportRefusal("malformed_receipt", fmt.Sprintf("an unresolved note is a line of text, not %s", reportType(obj["note"])))
			}
		}
		id, err := reportLine(strings.TrimSpace(id), "an unresolved id", 1<<30)
		if err != nil {
			return nil, err
		}
		note, err = reportLine(strings.TrimSpace(note), "an unresolved note", 1<<30)
		if err != nil {
			return nil, err
		}
		out = append(out, map[string]any{"id": id, "note": note})
	}
	return out, nil
}
func reportRepr(v any) string {
	switch x := v.(type) {
	case string:
		return store.PythonRepr(x)
	case map[string]any:
		parts := make([]string, 0, len(x))
		for key, value := range x {
			parts = append(parts, store.PythonRepr(key)+": "+reportRepr(value))
		}
		sort.Strings(parts)
		return "{" + strings.Join(parts, ", ") + "}"
	case []any:
		parts := make([]string, len(x))
		for i, value := range x {
			parts[i] = reportRepr(value)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case bool:
		if x {
			return "True"
		}
		return "False"
	default:
		return mergeevidence.Repr(v)
	}
}
