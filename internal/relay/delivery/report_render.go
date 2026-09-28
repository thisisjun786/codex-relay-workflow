package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

type reportSection struct {
	name       string
	lines      []string
	rank, keep int
	essential  bool
	last       bool
}

func reportString(value any) string { return py.Text(value) }
func reportValue(row map[string]any, key string) string {
	if row[key] == nil {
		return ""
	}
	return reportString(row[key])
}
func reportField(record Obj, key string) any { v, _ := get(record, key); return v }
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
func reportCompose(sections []reportSection, event string, budget int) (string, error) {
	blocks := make([][]string, len(sections))
	order := make([]int, len(sections))
	removed := []string{}
	lineCount, totalBytes := 0, 0
	for _, section := range sections {
		lineCount += len(section.lines)
		for _, line := range section.lines {
			totalBytes += len(line)
		}
	}
	tail := -1
	for i, section := range sections {
		blocks[i] = append([]string{}, section.lines...)
		order[i] = i
		if section.last {
			tail = i
		}
	}
	sort.SliceStable(order, func(i, j int) bool { return sections[order[i]].rank > sections[order[j]].rank })
	omission := func() string {
		return "omitted: " + strings.Join(removed, ", ") + " - read in full with codex-session-relay show --event " + event
	}
	rendered := func() string {
		lines := []string{}
		for i, block := range blocks {
			if i != tail {
				lines = append(lines, block...)
			}
		}
		if len(removed) > 0 {
			lines = append(lines, "", omission())
		}
		if tail >= 0 {
			lines = append(lines, blocks[tail]...)
		}
		return strings.Join(lines, "\n")
	}
	overBudget := func() bool {
		count, total := lineCount, totalBytes
		if len(removed) > 0 {
			count += 2
			total += len(omission())
		}
		return total+max(count-1, 0) > budget
	}
	for _, i := range order {
		if !overBudget() {
			break
		}
		if sections[i].essential || len(blocks[i]) == 0 {
			continue
		}
		removed = append(removed, sections[i].name)
		lineCount -= len(blocks[i])
		for _, line := range blocks[i] {
			totalBytes -= len(line)
		}
		blocks[i] = nil
	}
	for _, i := range order {
		section := sections[i]
		if !section.essential || len(section.lines) <= section.keep {
			continue
		}
		kept := len(section.lines)
		markerBytes := 0
		for overBudget() && kept > section.keep {
			kept--
			lineCount--
			totalBytes -= len(section.lines[kept])
			marker := fmt.Sprintf("  ... %d more, see the full record", len(section.lines)-kept)
			totalBytes += len(marker) - markerBytes
			if markerBytes == 0 {
				lineCount++
			}
			markerBytes = len(marker)
			blocks[i] = append(append([]string{}, section.lines[:kept]...), marker)
			if !reportContains(removed, section.name) {
				removed = append(removed, section.name)
			}
		}
	}
	text := rendered()
	if len(text) > budget {
		return "", fmt.Errorf("a message budget of %d bytes cannot hold this report even reduced to its required parts; raise the budget rather than shipping a message that lost them", budget)
	}
	return text, nil
}
func workRestoreLines(r map[string]any) []string {
	restoreLines := []string{}
	if py.Truthy(r["restore"]) {
		restore := py.Dict(r["restore"], false)
		restoreLines = []string{"", "workflow restore:"}
		for _, entry := range [][2]string{{"mode", "mode"}, {"scope", "scope"}, {"phase", "phase"}, {"phaseObservedAt", "phase observed"}, {"plan", "plan"}, {"evidence", "evidence"}, {"remaining", "remaining"}} {
			if value := reportValue(restore, entry[0]); py.Truthy(restore[entry[0]]) {
				restoreLines = append(restoreLines, "  "+entry[1]+": "+value)
			}
		}
		pointers := map[string]string{"development": "codexclaw:cxc-dev", "loop": "codexclaw:cxc-loop with codexclaw:cxc-pabcd", "lost-context": "codexclaw:cxc-lost-context", "pull-request": "codexclaw:cxc-dev references/stacked-prs.md", "review-repair": "codexclaw:cxc-review-repair"}
		for _, raw := range py.Items(restore["skills"]) {
			py.HashKey(raw)
			if pointer := pointers[reportString(raw)]; pointer != "" {
				restoreLines = append(restoreLines, "  read: "+pointer)
			} else {
				panic(&py.PythonError{Class: "KeyError", Detail: py.Repr(py.Repr(raw) + " has no recorded owner; name the owning skill rather than sending a recipient to reload everything")})
			}
		}
	}
	return restoreLines
}
func reportContains(values []string, word string) bool {
	for _, v := range values {
		if v == word {
			return true
		}
	}
	return false
}
func composeWorkCompletion(ctx context.Context, s *store.Store, row Row, receipt Obj, request string, r map[string]any, budget int) (string, error) {
	event := row.S("event_id")
	rid := row.S("relationship_id")
	purpose := "completion"
	switch reportField(receipt, "outcome") {
	case "ready_for_review":
		purpose = "review_ready"
	case "blocked_needs_input":
		purpose = "blocked"
	}
	digest := sha256.Sum256([]byte("child_to_parent|" + rid + "|" + purpose + "|" + event))
	messageID := hex.EncodeToString(digest[:])[:32]
	kind := "request"
	if purpose == "progress" {
		kind = "notification"
	}
	relationship, err := s.One(ctx, "SELECT * FROM relationships WHERE relationship_id = ?", rid)
	if err != nil {
		return "", err
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
			return "", err
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
	recipient := row.S("recipient_task_id")
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
				line := "  " + py.Text(check)
				if v["exitCode"] != nil {
					line += " -> exit " + reportString(v["exitCode"])
				}
				if py.Truthy(v["detail"]) {
					line += "  " + py.Text(v["detail"])
				}
				evidence = append(evidence, line)
			}
		}
	} else {
		evidence = []string{"", "verification: none recorded"}
	}
	unresolved := []string{"", "unresolved: none"}
	if items := py.Items(r["unresolved"]); len(items) > 0 {
		unresolved = []string{"", "unresolved:"}
		for _, raw := range items {
			if v, ok := raw.(string); ok {
				unresolved = append(unresolved, "  - "+unheaded(v))
			} else {
				v := py.Dict(raw, false)
				unresolved = append(unresolved, strings.TrimRight("  - "+unheaded(py.Text(v["id"]))+": "+py.Text(py.Or(v["note"], "")), ": "))
			}
		}
	}
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
	handoff := []string{}
	confirmations := []string{}
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
				panic(&py.PythonError{Class: "TypeError", Detail: fmt.Sprintf("sequence item %d: expected str instance, %s found", i, py.TypeName(item))})
			}
			names = append(names, name)
		}
		declared := "none declared by the branch"
		if len(names) > 0 {
			declared = strings.Join(names, ", ")
		}
		handoff = []string{"", "merge readiness (restate these; do not collect them again):", "  head " + py.Text(r["head_sha"]) + " on base " + py.Text(r["base_sha"]) + " verified " + py.Text(h["baseVerifiedAt"]), "  required: " + declared + fmt.Sprintf(" - %d run(s) restated", py.Len(checks)), fmt.Sprintf("  review: %s thread(s) seen over %s page(s), %s unresolved", reportString(coverage["totalCount"]), reportString(coverage["pagesRead"]), reportString(coverage["unresolved"]))}
		for _, raw := range py.Items(h["thread_dispositions"]) {
			item := py.Dict(raw, false)
			if item["disposition"] == "accepted" {
				confirmations = append(confirmations, fmt.Sprintf("    %s: %s - %s owns it, reopens on %s", py.Text(item["threadId"]), py.Text(item["addressedBy"]), py.Text(item["followUpOwner"]), py.Text(item["reopenTrigger"])))
			}
		}
		if len(confirmations) > 0 {
			confirmations = append([]string{fmt.Sprintf("  accepted by your decision (%d) - confirm each was yours:", len(confirmations))}, confirmations...)
		}
	}
	manifest := []string{"", "deliverables: none (execution-only outcome)"}
	if value := reportField(receipt, "manifest"); py.Truthy(value) {
		manifest = []string{"", fmt.Sprintf("deliverables: %d", py.Len(value))}
		for _, raw := range py.Items(value) {
			item := py.Dict(raw, false)
			line := "  " + py.Text(py.Item(raw, "path")) + "  sha256=" + py.Text(py.Item(raw, "sha256"))
			if size := item["bytes"]; size != nil {
				line += "  bytes=" + pyStr(size)
			}
			manifest = append(manifest, line)
		}
	}
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
	meaning := map[string]string{"DONE": "the child proved every recorded criterion against its own work", "NOOP": "nothing needed doing, and the finding that established that is the deliverable", "BLOCKED": "an external dependency is in the way", "UNSAFE": "a human risk decision is required before this can proceed", "NEEDS_HUMAN": "a judgment only the user can make", "BUDGET_EXHAUSTED": "a bound the plan actually stated ran out; best-so-far is adopted"}
	nonVerification := "a child report is the child describing its own execution, not a verdict"
	if r["cxc_status"] == "DONE" {
		nonVerification = "a DONE report is the child proving its own criteria, not the parent's verdict"
	}
	restoreLines := workRestoreLines(r)
	sections := []reportSection{
		{"header", []string{"[codex-session-relay] verification request", "result: " + reportValue(r, "summary"), "cxc: " + reportValue(r, "cxc_status") + " - " + reportValue(r, "cxc_reason"), "  meaning: " + meaning[reportValue(r, "cxc_status")], "  this is the child reporting on its own work. It is not a verification: " + nonVerification}, 0, 5, true, false},
		{"pull request", pr, 1, 2, true, false}, {"merge readiness", handoff, 1, 2, true, false}, {"acceptance confirmations", confirmations, 1, len(confirmations), true, false}, {"verification", evidence, 4, 0, false, false}, {"unresolved", unresolved, 2, 2, true, false}, {"next", []string{"next: " + reportValue(r, "next_action")}, 0, 1, true, false}, {"workflow restore", restoreLines, 3, 0, false, false}, {"deliverables", manifest, 6, 0, false, false}, {"manifest reference", manifestRef, 2, 2, true, false},
		{"relay record", []string{"", "relay record:", "  requestId: " + request, "  eventId: " + event, fmt.Sprintf("  submission: %d  contract: relay-report/1", r["submission_no"]), "  message: " + kind + " - an answer is owed by the recipient", "  messageId: " + messageID + "  child_to_parent/" + purpose + "  envelope: relay-envelope/1", "  relationshipId: " + rid, "  from: child " + sender + "  to: parent " + recipient, "  scope: " + scope, "  observedAt: " + reportValue(r, "recorded_at"), "  executionGeneration: " + pyStr(reportField(receipt, "executionGeneration")), "  attempt: " + pyStr(reportField(receipt, "attempt")), "  outcome: " + pyStr(reportField(receipt, "outcome")), "  revisionHash: " + pyStr(reportField(receipt, "revisionHash"))}, 5, 7, true, false},
		{"respond", []string{"", "To respond, from inside your own turn:", "  claim     --event " + event + " --turn <your turn id>", "  ack-proof --event " + event + " --turn <your turn id>", "  ack       --event " + event + " --ack-turn <your turn id> --ack-proof <proof>", "  verdict   --event " + event + " --verdict <verified|needs_changes|unverified|aborted> --verdict-turn <your turn id>", "", "The proof is sha256(eventId|<your own turn id>). This message does not and cannot", "contain that turn id, which is what distinguishes acknowledging from echoing.", "Full record: codex-session-relay show --event " + event}, 0, 7, true, false},
	}
	return reportCompose(sections, event, budget)
}
