package delivery

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func reportString(value any) string { return pyvalue.Str(value) }
func reportValue(row map[string]any, key string) string {
	if row[key] == nil {
		return ""
	}
	return reportString(row[key])
}
func reportField(record Obj, key string) any { v, _ := record.Lookup(key); return v }
func reportRows(ctx context.Context, s *store.Store, event string) (map[string]any, error) {
	row, err := s.One(ctx, "SELECT * FROM work_reports WHERE event_id = ? ORDER BY submission_no DESC LIMIT 1", event)
	if err != nil || row == nil {
		return nil, err
	}
	out := map[string]any{}
	for _, col := range row {
		out[col.Name] = col.Value
	}
	for _, key := range []string{"evidence", "unresolved", "review", "restore"} {
		if raw, ok := out[key].(string); ok {
			out[key] = py.Decode(raw)
		}
	}
	handoff, err := s.One(ctx, "SELECT * FROM work_report_handoffs WHERE event_id = ? AND submission_no = ?", event, row.Get("submission_no"))
	if err != nil {
		return nil, err
	}
	if handoff != nil {
		h := map[string]any{"isDraft": handoff.Get("is_draft") == int64(1), "baseVerifiedAt": handoff.Get("base_verified_at")}
		for _, name := range []string{"required_declared", "checks", "review_coverage", "thread_dispositions", "criterion_evidence", "limitations"} {
			if raw, ok := handoff.Get(name).(string); ok {
				h[name] = py.Decode(raw)
			}
		}
		out["handoff"] = h
	}
	return out, nil
}
func (d *Service) renderWorkReport(ctx context.Context, row Row, record Obj, request string) (text string, err error) {
	defer py.RecoverPython(&err)
	r, err := reportRows(ctx, d.Store, row.S("event_id"))
	if err != nil {
		return "", err
	}
	if r == nil {
		return "", fmt.Errorf("report disappeared for %s", row.S("event_id"))
	}
	if row.S("kind") == Revision {
		return composeWorkRevision(row, record, request, r, 6000)
	}
	return composeWorkCompletion(ctx, d.Store, row, record, request, r, 6000)
}

// PreviewReport composes one report at a caller-specified byte budget, for contract checks.
func (d *Service) PreviewReport(ctx context.Context, event, request string, budget int) (text string, err error) {
	defer py.RecoverPython(&err)
	row, err := d.Get(ctx, event)
	if err != nil {
		return "", err
	}
	receipt, err := d.Receipt(ctx, event)
	if err != nil {
		return "", err
	}
	report, err := reportRows(ctx, d.Store, event)
	if err != nil {
		return "", err
	}
	if report == nil {
		return "", fmt.Errorf("no work report for %s", event)
	}
	if row.S("kind") == Revision {
		return composeWorkRevision(row, receipt, request, report, budget)
	}
	return composeWorkCompletion(ctx, d.Store, row, receipt, request, report, budget)
}

// composeWorkCompletion builds the verification request the parent reads. The section builders
// run in a fixed order because a stored value of the wrong shape fails where it is first read
// (a Python error that renderWorkReport returns): the first fault in this order is the one
// reported.
func composeWorkCompletion(ctx context.Context, s *store.Store, row Row, receipt Obj, request string, r map[string]any, budget int) (string, error) {
	event, rid := row.S("event_id"), row.S("relationship_id")
	purpose := completionPurpose(receipt)
	sender, scope, err := completionParties(ctx, s, rid)
	if err != nil {
		return "", err
	}
	evidence := completionVerificationLines(r)
	unresolved := unresolvedSection(r)
	pr := completionPullRequestLines(r)
	handoff, confirmations := completionHandoffLines(r)
	manifest := completionManifestLines(receipt)
	manifestRef := completionManifestRefLines(receipt)
	nonVerification := "a child report is the child describing its own execution, not a verdict"
	if r["cxc_status"] == "DONE" {
		nonVerification = "a DONE report is the child proving its own criteria, not the parent's verdict"
	}
	restore := restoreSection(r)
	sections := []reportSection{
		reportHeaderSection("[codex-session-relay] verification request", "result: "+reportValue(r, "summary"), r, "  this is the child reporting on its own work. It is not a verification: "+nonVerification),
		{"pull request", pr, 1, 2, true, false}, {"merge readiness", handoff, 1, 2, true, false}, {"acceptance confirmations", confirmations, 1, len(confirmations), true, false}, {"verification", evidence, 4, 0, false, false}, unresolved, {"next", []string{"next: " + reportValue(r, "next_action")}, 0, 1, true, false}, restore, {"deliverables", manifest, 6, 0, false, false}, {"manifest reference", manifestRef, 2, 2, true, false},
		completionRecordSection(row, receipt, request, r, purpose, sender, scope),
		respondSection(event),
	}
	return reportCompose(sections, event, budget)
}

// completionPurpose is what the message is for, as the receipt's outcome names it.
func completionPurpose(receipt Obj) string {
	purpose := "completion"
	switch reportField(receipt, "outcome") {
	case "ready_for_review":
		purpose = "review_ready"
	case "blocked_needs_input":
		purpose = "blocked"
	}
	return purpose
}

// completionParties reads who sent the report and the scope it was written under.
func completionParties(ctx context.Context, s *store.Store, rid string) (string, string, error) {
	relationship, err := s.One(ctx, "SELECT * FROM relationships WHERE relationship_id = ?", rid)
	if err != nil {
		return "", "", err
	}
	sender := "<unknown: the sending task was not read from the relationship>"
	scope := "<unknown: no Linear scope was read>"
	if relationship != nil {
		if id, ok := relationship.Get("child_task_id").(string); ok && id != "" {
			sender = id
		}
		issue, _ := relationship.Get("issue_key").(string)
		project, err := s.One(ctx, "SELECT project_key FROM relationship_scope WHERE relationship_id = ?", rid)
		if err != nil {
			return "", "", err
		}
		if issue != "" {
			if project != nil && project.Get("project_key") != nil {
				scope = "project " + pyStr(project.Get("project_key")) + ", issue " + issue
			} else {
				scope = "issue " + issue + "; no project scope is recorded for this relationship"
			}
		} else {
			scope = "<unknown: neither a project nor an issue scope was readable>"
		}
	}
	return sender, scope, nil
}

// completionVerificationLines are the evidence the child recorded, one line per check.
func completionVerificationLines(r map[string]any) []string {
	evidence := []string{"", "verification:"}
	if items := py.Items(r["evidence"]); len(items) > 0 {
		for _, raw := range items {
			if v, ok := raw.(string); ok {
				evidence = append(evidence, "  "+v)
			} else {
				v := py.Dict(raw, false)
				check, present := v["check"]
				if !present {
					check = ""
				}
				line := "  " + pyvalue.Str(check)
				if v["exitCode"] != nil {
					line += " -> exit " + reportString(v["exitCode"])
				}
				if pyvalue.Truthy(v["detail"]) {
					line += "  " + pyvalue.Str(v["detail"])
				}
				evidence = append(evidence, line)
			}
		}
	} else {
		evidence = []string{"", "verification: none recorded"}
	}
	return evidence
}

// completionPullRequestLines name the pull request and its base, head and criteria.
func completionPullRequestLines(r map[string]any) []string {
	pr := []string{"", "repository: " + reportValue(r, "repository"), "pull request: none recorded for this event"}
	if r["pr_number"] != nil {
		pr = []string{"", "pull request: " + reportValue(r, "repository") + "#" + pyStr(r["pr_number"])}
		if r["pr_state"] != nil && r["pr_state"] != "" {
			pr[1] += "  (" + reportValue(r, "pr_state") + ")"
		}
		if r["pr_url"] != nil && r["pr_url"] != "" {
			pr = append(pr, "  url: "+reportValue(r, "pr_url"))
		}
	}
	if r["base_sha"] != nil || r["base_ref"] != nil {
		pr = append(pr, strings.TrimRight("  base: "+reportValue(r, "base_ref")+" "+reportValue(r, "base_sha"), " "))
	}
	for _, pair := range [][2]string{{"head_sha", "head"}, {"criteria_digest", "criteria"}} {
		if r[pair[0]] != nil && r[pair[0]] != "" {
			pr = append(pr, "  "+pair[1]+": "+reportValue(r, pair[0]))
		}
	}
	return pr
}

// completionHandoffLines are the merge-readiness block and the accepted-thread confirmations.
func completionHandoffLines(r map[string]any) (handoff, confirmations []string) {
	handoff, confirmations = []string{}, []string{}
	if h, ok := r["handoff"].(map[string]any); ok {
		coverage := py.Dict(h["review_coverage"], true)
		checks := py.Or(h["checks"], []any{})
		requiredValue := py.Or(h["required_declared"], []any{})
		if _, isList := py.List(requiredValue); !isList {
			if _, isObject := py.Object(requiredValue); !isObject {
				if _, isText := requiredValue.(string); !isText {
					panic(&py.PythonError{Class: "TypeError", Detail: "can only join an iterable"})
				}
			}
		}
		required := py.Items(requiredValue)
		names := []string{}
		for i, item := range required {
			name, ok := item.(string)
			if !ok {
				panic(&py.PythonError{Class: "TypeError", Detail: fmt.Sprintf("sequence item %d: expected str instance, %s found", i, pyvalue.TypeName(item))})
			}
			names = append(names, name)
		}
		declared := "none declared by the branch"
		if len(names) > 0 {
			declared = strings.Join(names, ", ")
		}
		handoff = []string{"", "merge readiness (restate these; do not collect them again):", "  head " + pyvalue.Str(r["head_sha"]) + " on base " + pyvalue.Str(r["base_sha"]) + " verified " + pyvalue.Str(h["baseVerifiedAt"]), "  required: " + declared + fmt.Sprintf(" - %d run(s) restated", py.Len(checks)), fmt.Sprintf("  review: %s thread(s) seen over %s page(s), %s unresolved", reportString(coverage["totalCount"]), reportString(coverage["pagesRead"]), reportString(coverage["unresolved"]))}
		for _, raw := range py.Items(h["thread_dispositions"]) {
			item := py.Dict(raw, false)
			if item["disposition"] == "accepted" {
				confirmations = append(confirmations, fmt.Sprintf("    %s: %s - %s owns it, reopens on %s", pyvalue.Str(item["threadId"]), pyvalue.Str(item["addressedBy"]), pyvalue.Str(item["followUpOwner"]), pyvalue.Str(item["reopenTrigger"])))
			}
		}
		if len(confirmations) > 0 {
			confirmations = append([]string{fmt.Sprintf("  accepted by your decision (%d) - confirm each was yours:", len(confirmations))}, confirmations...)
		}
	}
	return handoff, confirmations
}

// completionManifestLines list the deliverables the receipt declared.
func completionManifestLines(receipt Obj) []string {
	manifest := []string{"", "deliverables: none (execution-only outcome)"}
	if value := reportField(receipt, "manifest"); pyvalue.Truthy(value) {
		manifest = []string{"", fmt.Sprintf("deliverables: %d", py.Len(value))}
		for _, raw := range py.Items(value) {
			item := py.Dict(raw, false)
			line := "  " + pyvalue.Str(py.Item(raw, "path")) + "  sha256=" + pyvalue.Str(py.Item(raw, "sha256"))
			if size := item["bytes"]; size != nil {
				line += "  bytes=" + pyStr(size)
			}
			manifest = append(manifest, line)
		}
	}
	return manifest
}

// completionManifestRefLines show the manifest reference, cut at 240 bytes on a character boundary.
func completionManifestRefLines(receipt Obj) []string {
	manifestRef := []string{}
	if ref := reportField(receipt, "manifestRef"); ref != nil && ref != "" {
		text := pyStr(ref)
		if !utf8.ValidString(text) || strings.Contains(text, `\ud800`) {
			manifestRef = []string{"", "manifestRef: present but not renderable; read it in the record"}
		} else {
			if len(text) > 240 {
				raw := []byte(text)[:240]
				for !utf8.Valid(raw) {
					raw = raw[:len(raw)-1]
				}
				text = string(raw) + "... (truncated; full value in the record)"
			}
			manifestRef = []string{"", "manifestRef: " + text}
		}
	}
	return manifestRef
}

// completionRecordSection is the relay record block: the ids, parties, scope and receipt identity.
func completionRecordSection(row Row, receipt Obj, request string, r map[string]any, purpose, sender, scope string) reportSection {
	event, rid, recipient := row.S("event_id"), row.S("relationship_id"), row.S("recipient_task_id")
	messageID := reportMessageID("child_to_parent", rid, purpose, event)
	kind := "request"
	if purpose == "progress" {
		kind = "notification"
	}
	return reportSection{"relay record", []string{"", "relay record:", "  requestId: " + request, "  eventId: " + event, fmt.Sprintf("  submission: %d  contract: relay-report/1", r["submission_no"]), "  message: " + kind + " - an answer is owed by the recipient", "  messageId: " + messageID + "  child_to_parent/" + purpose + "  envelope: relay-envelope/1", "  relationshipId: " + rid, "  from: child " + sender + "  to: parent " + recipient, "  scope: " + scope, "  observedAt: " + reportValue(r, "recorded_at"), "  executionGeneration: " + pyStr(reportField(receipt, "executionGeneration")), "  attempt: " + pyStr(reportField(receipt, "attempt")), "  outcome: " + pyStr(reportField(receipt, "outcome")), "  revisionHash: " + pyStr(reportField(receipt, "revisionHash"))}, 5, 7, true, false}
}

// respondSection tells the recipient how to answer.
func respondSection(event string) reportSection {
	return reportSection{"respond", []string{"", "To respond, from inside your own turn:", "  claim     --event " + event + " --turn <your turn id>", "  ack-proof --event " + event + " --turn <your turn id>", "  ack       --event " + event + " --ack-turn <your turn id> --ack-proof <proof>", "  verdict   --event " + event + " --verdict <verified|needs_changes|unverified|aborted> --verdict-turn <your turn id>", "", "The proof is sha256(eventId|<your own turn id>). This message does not and cannot", "contain that turn id, which is what distinguishes acknowledging from echoing.", "Full record: codex-session-relay show --event " + event}, 0, 7, true, false}
}
