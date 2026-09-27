package delivery

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"

	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func composeWorkRevision(row Row, receipt Obj, request string, report map[string]any, budget int) (string, error) {
	text, _, _, err := composeWorkRevisionMeasured(row, receipt, request, report, budget)
	return text, err
}

func composeWorkRevisionMeasured(row Row, receipt Obj, request string, report map[string]any, budget int) (string, []string, bool, error) {
	event, rid := row.S("event_id"), row.S("relationship_id")
	review := py.Dict(report["review"], true)
	findings := correctionFindings(receipt)
	extras := map[string]map[string]any{}
	orderedExtras := []string{}
	for _, raw := range py.Items(review["findings"]) {
		id := py.Item(raw, "id")
		py.HashKey(id)
		item := py.Dict(raw, false)
		name := py.Text(id)
		if _, seen := extras[name]; !seen {
			orderedExtras = append(orderedExtras, name)
		}
		extras[name] = item
	}
	// The review supplies the correction only when the verdict named no findings.
	source := receipt
	if len(findings) == 0 && len(orderedExtras) > 0 {
		items := []any{}
		for _, id := range orderedExtras {
			item := extras[id]
			items = append(items, Obj{{Key: "id", Value: id}, {Key: "verdict", Value: item["verdict"]}})
		}
		source = set(append(Obj(nil), receipt...), "criteria", items)
	}
	heading := violatedHeading(source)
	if len(findings) == 0 && len(orderedExtras) > 0 {
		heading = strings.Replace(heading, "of the "+findingsWord(len(orderedExtras), "recorded finding")+",", fmt.Sprintf("none recorded by the verdict; the parent's review names %d:", len(orderedExtras)), 1)
	}
	findingLines := []string{"", heading}
	findingOwners := map[int]string{}
	seen := map[string]bool{}
	for _, item := range findings {
		id := py.Text(reportField(item, "id"))
		if !py.Truthy(reportField(item, "id")) {
			continue
		}
		seen[id] = true
		extra := extras[id]
		note := reportField(item, "note")
		if !py.Truthy(note) {
			note = extra["note"]
		}
		noteText := noNote
		if py.Truthy(note) {
			noteText = inline(note)
		}
		disposition := reportField(item, "verdict")
		line := "  " + unheaded(inline(id)) + restorationLabel(item)
		if disposition != nil && disposition != "" {
			line += ": " + inline(disposition)
		}
		line += " - " + noteText
		findingOwners[len(findingLines)] = id
		findingLines = append(findingLines, line)
		anchor := extra["anchor"]
		if !py.Truthy(anchor) {
			anchor = reportField(item, "anchor")
		}
		if py.Truthy(anchor) {
			findingLines = append(findingLines, "    anchor: "+inline(anchor))
		}
	}
	reviewOnly := []string{}
	for _, id := range orderedExtras {
		if !seen[id] {
			reviewOnly = append(reviewOnly, id)
		}
	}
	if len(reviewOnly) > 0 {
		if len(findings) > 0 {
			findingLines = append(findingLines, "  also raised in review, not part of the recorded verdict:")
		} else {
			findingLines = append(findingLines, "  from the review; this assignment has no recorded criteria set:")
		}
	}
	for _, id := range reviewOnly {
		item := extras[id]
		note := noNote
		if py.Truthy(item["note"]) {
			note = inline(item["note"])
		}
		line := "  " + unheaded(inline(id))
		if item["verdict"] != nil && item["verdict"] != "" {
			line += ": " + inline(item["verdict"])
		}
		findingLines = append(findingLines, line+" - "+note)
		if py.Truthy(item["anchor"]) {
			findingLines = append(findingLines, "    anchor: "+inline(item["anchor"]))
		}
	}
	if len(findings) == 0 && len(reviewOnly) == 0 {
		findingLines = []string{"", violatedHeading(receipt)}
	}
	what := whatChangedLines(receipt)[:2]
	if len(review) > 0 {
		if kind := review["kind"]; py.Truthy(kind) {
			what[1] += "; the review judged " + py.Text(kind)
			if _, ok := py.PyInt(review["blockers"]); ok {
				count := review["blockers"]
				n := reportString(count)
				what[1] += " with " + n + " blocker"
				if n != "1" {
					what[1] += "s"
				}
			}
		}
	}
	fix := fixScopeLines(source)
	if len(fix) > 2 {
		fix = fix[:len(fix)-1]
	}
	if len(findings) > 0 && py.Truthy(report["review"]) {
		fix[1] = strings.Replace(fix[1], "verified and unverified findings are out of scope", "verified, unverified and review-only findings are out of scope", 1)
	}
	scope := []string{"", "SCOPE:", "  " + unheaded(reportValue(report, "repository"))}
	if report["pr_number"] != nil {
		scope[2] += "#" + reportString(report["pr_number"])
	}
	if report["base_sha"] != nil {
		scope = append(scope, strings.TrimRight("  base "+reportValue(report, "base_ref")+" "+reportValue(report, "base_sha"), " "))
	}
	if report["head_sha"] != nil {
		scope = append(scope, "  head "+reportValue(report, "head_sha"))
	}
	if report["criteria_digest"] != nil {
		scope = append(scope, "  criteria "+reportValue(report, "criteria_digest"))
	}
	scope = append(scope, "  execution generation "+known(reportField(receipt, "executionGeneration"))+" (new)")
	proof := []string{"", "PROOF:"}
	if evidence := py.Items(report["evidence"]); len(evidence) > 0 {
		proof = append(proof, "  re-run what this review ran, and report command, exit code and result:")
		for _, raw := range evidence {
			if item, ok := raw.(string); ok {
				proof = append(proof, "    "+unheaded(item))
			} else {
				item := py.Dict(raw, false)
				proof = append(proof, "    "+unheaded(py.Text(item["check"])))
			}
		}
	} else {
		proof = append(proof, "  state the command, its exit code and what it showed, for each finding FIX SCOPE or REVERIFY AND RETURN names")
	}
	proof = append(proof, "  a passing string match is not a passing behaviour")
	reverify := reverifyLines(source)
	if len(correctionFindings(source)) > 0 {
		reverify[1] = strings.Replace(reverify[1], "re-check each finding FIX SCOPE names", "re-check each finding FIX SCOPE names (PROOF says how)", 1)
	}
	reverify = append(reverify, returnLines(rid, reportField(receipt, "executionGeneration"))...)
	unresolved := []string{"", "unresolved: none"}
	if entries := py.Items(report["unresolved"]); len(entries) > 0 {
		unresolved = []string{"", "unresolved:"}
		for _, raw := range entries {
			if item, ok := raw.(string); ok {
				unresolved = append(unresolved, "  - "+unheaded(item))
			} else {
				item := py.Dict(raw, false)
				unresolved = append(unresolved, strings.TrimRight("  - "+unheaded(py.Text(item["id"]))+": "+py.Text(py.Or(item["note"], "")), ": "))
			}
		}
	}
	digest := sha256.Sum256([]byte("parent_to_child|" + rid + "|revision_request|" + event))
	messageID := hex.EncodeToString(digest[:])[:32]
	meaning := map[string]string{"DONE": "the child proved every recorded criterion against its own work", "NOOP": "nothing needed doing, and the finding that established that is the deliverable", "BLOCKED": "an external dependency is in the way", "UNSAFE": "a human risk decision is required before this can proceed", "NEEDS_HUMAN": "a judgment only the user can make", "BUDGET_EXHAUSTED": "a bound the plan actually stated ran out; best-so-far is adopted"}
	sections := []reportSection{
		{"header", []string{"[codex-session-relay] revision request", "TASK: the parent's summary (FIX SCOPE bounds it): " + reportValue(report, "summary"), "cxc: " + reportValue(report, "cxc_status") + " - " + reportValue(report, "cxc_reason"), "  meaning: " + meaning[reportValue(report, "cxc_status")]}, 0, 4, true, false},
		{"VIOLATED CRITERION", findingLines, 0, 2, true, false}, {"WHAT CHANGED", what, 0, len(what), true, false}, {"SCOPE", scope, 1, 2, true, false}, {"FIX SCOPE", fix, 0, len(fix), true, false},
		{"PRESERVE", []string{"PRESERVE: everything FIX SCOPE does not name, verified findings included,", "  work this request does not mention and any other task in-flight beside it"}, 0, 2, true, false},
		{"unresolved", unresolved, 2, 2, true, false},
		{"MUST DO", []string{"MUST DO:", "  the parent's next action, as written: " + reportValue(report, "next_action"), "  where it goes beyond FIX SCOPE, FIX SCOPE and DECISION BOUNDARY decide", "  answer every finding FIX SCOPE names with a fix, a reasoned rebuttal, or an explicit out-of-scope split, and reply on its thread; supply the evidence REVERIFY AND RETURN asks for"}, 0, 3, true, false},
		{"MUST NOT", []string{"MUST NOT:", "  discard work FIX SCOPE does not name, rewrite another task history, or force-push a shared branch", "  treat this request as an acknowledgeable message; see the note below"}, 1, 2, true, false},
		{"PROOF", proof, 2, 2, true, false}, {"RETURN FORMAT", []string{"RETURN FORMAT:", "  result summary, repository and pull request, base and head SHA, verification evidence, unresolved items, next action, and the CXC report status"}, 2, 2, true, false},
		{"DECISION BOUNDARY", []string{"DECISION BOUNDARY:", "  fix what FIX SCOPE names. Anything wider, anything that would discard preserved work, and anything needing authority you were not given comes back here instead of being decided locally"}, 1, 2, true, false},
		{"workflow restore", workRestoreLines(report), 3, 0, false, false}, {"REVERIFY AND RETURN", reverify, 0, len(reverify), true, false},
		{"relay record", []string{"", fmt.Sprintf("relay record: requestId %s, eventId %s, submission %v, contract relay-report/1", request, event, report["submission_no"]), "  message: request parent_to_child/revision_request, messageId " + messageID + ", envelope relay-envelope/1", "Full record: codex-session-relay show --event " + event}, 0, 4, true, false},
	}
	if py.Truthy(report["review"]) {
		kind := py.Item(report["review"], "kind")
		line := "VERDICT: " + py.Text(kind)
		if !py.Equal(kind, "PASS") && !py.Equal(kind, "FAIL") && !py.Equal(kind, "GO-WITH-FIXES") {
			panic(&py.PythonError{Class: "ValueError", Detail: py.Repr(kind) + " is not a review verdict; REVIEW-OUTPUT-01 fixes PASS, GO-WITH-FIXES, FAIL"})
		}
		if kind == "GO-WITH-FIXES" {
			n, ok := py.PyInt(review["blockers"])
			if !ok || n < 1 {
				panic(&py.PythonError{Class: "ValueError", Detail: "GO-WITH-FIXES states how many blockers it is going ahead with; a count below one is a PASS and should say so"})
			}
			if n > 9999 {
				panic(&py.PythonError{Class: "ValueError", Detail: fmt.Sprintf("a blocker count of %d is past the point of being a review, and it sits on a line the message cannot shorten; the limit is 9999", n)})
			}
			line += fmt.Sprintf(" (blockers=%d)", n)
		} else if review["blockers"] != nil {
			panic(&py.PythonError{Class: "ValueError", Detail: "a " + py.Text(kind) + " verdict carries no blocker count"})
		}
		sections = append(sections, reportSection{"verdict", []string{"", line}, 0, 2, true, true})
	}
	text, err := reportCompose(sections, event, budget)
	if err != nil {
		return "", nil, false, err
	}
	// reportCompose removes only suffixes from essential sections. Compare the
	// retained prefix at its actual section offset, not substrings in parent text.
	lines := strings.Split(text, "\n")
	survivors := []string{}
	for i, line := range findingLines {
		if 4+i >= len(lines) || lines[4+i] != line {
			break
		}
		if id, ok := findingOwners[i]; ok {
			survivors = append(survivors, id)
		}
	}
	restoreDropped := false
	for _, line := range lines {
		if strings.HasPrefix(line, "omitted: ") {
			names, _, _ := strings.Cut(strings.TrimPrefix(line, "omitted: "), " - read in full with ")
			for _, name := range strings.Split(names, ", ") {
				if name == "workflow restore" {
					restoreDropped = true
				}
			}
		}
	}
	return text, survivors, restoreDropped, nil
}

// reportProjectionBudget is report.BUDGET; tests may squeeze it just as Python's
// restoration test patches that value. Normal rendering keeps its own default.
var reportProjectionBudget = 6000

// ProjectReportRestoration measures the candidate report already inserted by its
// caller. Call it inside RecordWorkReport's write transaction with that transaction's
// ctx, before COMMIT; all reads use the Store's ctx-aware querier, so the candidate
// and the latest delivery attempt count are measured under the same write lock.
// A returned error must roll back the candidate. A non-nil result is journalled as
// restoration_rendered and attached to the returned report by the caller.
func ProjectReportRestoration(ctx context.Context, s *store.Store, eventID string) (map[string]any, error) {
	event, err := one(ctx, s, "SELECT * FROM events WHERE event_id = ?", eventID)
	if err != nil || event == nil || event.S("outcome") != Revision {
		return nil, err
	}
	receipt := loadsObj(event.S("receipt"))
	findings := correctionFindings(receipt)
	index := -1
	var block Obj
	for i, f := range findings {
		if v, _ := get(f, "restoration"); truthy(v) {
			index, block = i, f
			break
		}
	}
	result := func(outcome string, criterion any, detail string) map[string]any {
		return map[string]any{"outcome": outcome, "basis": "relay-report/1", "criterion": criterion, "detail": detail}
	}
	if block == nil {
		return result("not_carried", nil, "no finding declared a restoration block"), nil
	}
	row, err := one(ctx, s, `SELECT d.*, EXISTS(SELECT 1 FROM relationships r WHERE r.relationship_id=d.relationship_id AND r.superseded_by IS NULL) AS relationship_live, NOT EXISTS(SELECT 1 FROM delivery_supersession ds WHERE ds.event_id=d.event_id) AS correction_open, EXISTS(SELECT 1 FROM events e JOIN relationships r ON r.relationship_id=e.relationship_id WHERE e.event_id=d.event_id AND e.stage='final' AND e.execution_generation>=r.execution_generation) AS event_current FROM deliveries d WHERE d.event_id=?`, eventID)
	if err != nil {
		return nil, err
	}
	unclaimable := ""
	switch {
	case row == nil:
		unclaimable = "no delivery is queued for it"
	case !row.N("hold_reason"):
		unclaimable = "the delivery is held: " + store.PyRepr(row.S("hold_reason"))
	case row.I("relationship_live") == 0:
		unclaimable = "its relationship has been superseded"
	case row.I("correction_open") == 0:
		unclaimable = "the correction it belongs to has already been answered and superseded"
	case row.I("event_current") == 0:
		unclaimable = "the event is behind the assignment's current generation"
	case row.S("state") != Queued && row.S("state") != DeferredBusy && row.S("state") != WithheldPreSend && row.S("state") != Sending && row.S("state") != HeldUncertain:
		unclaimable = "the delivery is " + store.PyRepr(row.S("state"))
	}
	if unclaimable != "" {
		return result("unmeasured", nil, "no further attempt will render this report; "+unclaimable), nil
	}
	report, err := reportRows(ctx, s, eventID)
	if err != nil {
		return nil, err
	}
	attempt := row.I("attempt_count") + 1
	request, err := store.RequestID(eventID, int(attempt))
	if err != nil {
		return nil, err
	}
	_, survivors, restoreDropped, err := composeWorkRevisionMeasured(row, receipt, request, report, reportProjectionBudget)
	id := reportField(block, "id")
	if err != nil {
		return nil, refuse(RestorationUndeliverable, "this report cannot be composed into a revision message, so the restoration block on %s cannot travel and every delivery attempt would fail the same way inside its claim: %s", pyReprValue(id), err)
	}
	present := false
	for _, survivor := range survivors {
		if survivor == id {
			present = true
		}
	}
	where := fmt.Sprintf("finding %d of %d", index+1, len(findings))
	if !present {
		return nil, refuse(RestorationUndeliverable, "this report would push the restoration block on %s out of the correction: %s, removed while fitting the message to its byte budget. Shorten the report or move the block and record it again. Afterwards there is no supported way to send the block: the verdict does not resend and a second channel is not allowed", pyReprValue(id), where)
	}
	projection := result("carried", id, where+", still present after the message was fitted")
	projection["attempt"] = attempt
	projection["restoreSection"] = "absent"
	if py.Truthy(report["restore"]) {
		projection["restoreSection"] = "carried"
		if restoreDropped {
			projection["restoreSection"] = "budget_dropped"
		}
	}
	return projection, nil
}
