package delivery

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyvalue"
	py "github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
)

// composeWorkRevision builds the revision request the child reads. The section builders run in a
// fixed order because a stored value of the wrong shape fails where it is first read (a Python
// error that renderWorkReport returns): the first fault in this order is the one reported, and the
// restore block, which sits inside the section list, is read before the review verdict is checked.
func composeWorkRevision(row Row, receipt Obj, request string, report map[string]any, budget int) (string, error) {
	event, rid := row.S("event_id"), row.S("relationship_id")
	x := indexReview(receipt, report)
	violated := x.violated(receipt)
	what := x.whatChanged(receipt)
	fix := x.fixScope(report)
	scope := revisionScopeLines(report, receipt)
	proof := revisionProofLines(report)
	reverify := x.reverify(rid, receipt)
	unresolved := unresolvedSection(report)
	messageID := reportMessageID("parent_to_child", rid, "revision_request", event)
	sections := []reportSection{
		reportHeaderSection("[codex-session-relay] revision request", "TASK: the parent's summary (FIX SCOPE bounds it): "+reportValue(report, "summary"), report),
		{"VIOLATED CRITERION", violated, 0, 2, true, false}, {"WHAT CHANGED", what, 0, len(what), true, false}, {"SCOPE", scope, 1, 2, true, false}, {"FIX SCOPE", fix, 0, len(fix), true, false},
		{"PRESERVE", []string{"PRESERVE: everything FIX SCOPE does not name, verified findings included,", "  work this request does not mention and any other task in-flight beside it"}, 0, 2, true, false},
		unresolved,
		{"MUST DO", []string{"MUST DO:", "  the parent's next action, as written: " + reportValue(report, "next_action"), "  where it goes beyond FIX SCOPE, FIX SCOPE and DECISION BOUNDARY decide", "  answer every finding FIX SCOPE names with a fix, a reasoned rebuttal, or an explicit out-of-scope split, and reply on its thread; supply the evidence REVERIFY AND RETURN asks for"}, 0, 3, true, false},
		{"MUST NOT", []string{"MUST NOT:", "  discard work FIX SCOPE does not name, rewrite another task history, or force-push a shared branch", "  treat this request as an acknowledgeable message; see the note below"}, 1, 2, true, false},
		{"PROOF", proof, 2, 2, true, false}, {"RETURN FORMAT", []string{"RETURN FORMAT:", "  result summary, repository and pull request, base and head SHA, verification evidence, unresolved items, next action, and the CXC report status"}, 2, 2, true, false},
		{"DECISION BOUNDARY", []string{"DECISION BOUNDARY:", "  fix what FIX SCOPE names. Anything wider, anything that would discard preserved work, and anything needing authority you were not given comes back here instead of being decided locally"}, 1, 2, true, false},
		restoreSection(report), {"REVERIFY AND RETURN", reverify, 0, len(reverify), true, false},
		{"relay record", []string{"", fmt.Sprintf("relay record: requestId %s, eventId %s, submission %v, contract relay-report/1", request, event, report["submission_no"]), "  message: request parent_to_child/revision_request, messageId " + messageID + ", envelope relay-envelope/1", "Full record: codex-session-relay show --event " + event}, 0, 4, true, false},
	}
	return reportCompose(append(sections, x.verdictSections(report)...), event, budget)
}

// reviewIndex is what the revision request derives once from the stored verdict and the parent's
// review: the review's findings by id, and the receipt that carries the criteria the sections
// read (the review's findings stand in for them when the verdict named none).
type reviewIndex struct {
	review        map[string]any
	findings      []Obj
	extras        map[string]map[string]any
	orderedExtras []string
	source        Obj
}

// indexReview reads the review once. It raises, as the stored value would in Python, when the review
// or one of its findings has the wrong shape.
func indexReview(receipt Obj, report map[string]any) reviewIndex {
	review := py.Dict(report["review"], true)
	findings := correctionFindings(receipt)
	extras := map[string]map[string]any{}
	orderedExtras := []string{}
	for _, raw := range py.Items(review["findings"]) {
		id := py.Item(raw, "id")
		py.HashKey(id)
		item := py.Dict(raw, false)
		name := pyvalue.Str(id)
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
		source = append(Obj(nil), receipt...).Set("criteria", items)
	}
	return reviewIndex{review, findings, extras, orderedExtras, source}
}

// violated is the VIOLATED CRITERION section: the verdict's findings, then the review's own.
func (x reviewIndex) violated(receipt Obj) []string {
	findingLines := []string{"", x.violatedHeading()}
	recorded, seen := x.recordedFindingLines()
	findingLines = append(findingLines, recorded...)
	reviewOnly := x.reviewOnlyIDs(seen)
	findingLines = append(findingLines, x.reviewOnlyLines(reviewOnly)...)
	if len(x.findings) == 0 && len(reviewOnly) == 0 {
		findingLines = []string{"", violatedHeading(receipt)}
	}
	return findingLines
}

// violatedHeading is the heading of VIOLATED CRITERION; with no recorded finding it names the
// findings the parent's review carries.
func (x reviewIndex) violatedHeading() string {
	findings, orderedExtras, source := x.findings, x.orderedExtras, x.source
	heading := violatedHeading(source)
	if len(findings) == 0 && len(orderedExtras) > 0 {
		heading = strings.Replace(heading, "of the "+findingsWord(len(orderedExtras), "recorded finding")+",", fmt.Sprintf("none recorded by the verdict; the parent's review names %d:", len(orderedExtras)), 1)
	}
	return heading
}

// recordedFindingLines are the findings the verdict recorded, with the ids they used.
func (x reviewIndex) recordedFindingLines() ([]string, map[string]bool) {
	findings, extras := x.findings, x.extras
	findingLines := []string{}
	seen := map[string]bool{}
	for _, item := range findings {
		id := pyvalue.Str(reportField(item, "id"))
		if !pyvalue.Truthy(reportField(item, "id")) {
			continue
		}
		seen[id] = true
		extra := extras[id]
		note := reportField(item, "note")
		if !pyvalue.Truthy(note) {
			note = extra["note"]
		}
		noteText := noNote
		if pyvalue.Truthy(note) {
			noteText = inline(note)
		}
		disposition := reportField(item, "verdict")
		line := "  " + unheaded(inline(id)) + restorationLabel(item)
		if disposition != nil && disposition != "" {
			line += ": " + inline(disposition)
		}
		line += " - " + noteText
		findingLines = append(findingLines, line)
		anchor := extra["anchor"]
		if !pyvalue.Truthy(anchor) {
			anchor = reportField(item, "anchor")
		}
		if pyvalue.Truthy(anchor) {
			findingLines = append(findingLines, "    anchor: "+inline(anchor))
		}
	}
	return findingLines, seen
}

// reviewOnlyIDs are the review's finding ids the verdict did not record, in review order.
func (x reviewIndex) reviewOnlyIDs(seen map[string]bool) []string {
	orderedExtras := x.orderedExtras
	reviewOnly := []string{}
	for _, id := range orderedExtras {
		if !seen[id] {
			reviewOnly = append(reviewOnly, id)
		}
	}
	return reviewOnly
}

// reviewOnlyLines are the review findings the verdict did not record, under their own lead-in.
func (x reviewIndex) reviewOnlyLines(reviewOnly []string) []string {
	findings, extras := x.findings, x.extras
	findingLines := []string{}
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
		if pyvalue.Truthy(item["note"]) {
			note = inline(item["note"])
		}
		line := "  " + unheaded(inline(id))
		if item["verdict"] != nil && item["verdict"] != "" {
			line += ": " + inline(item["verdict"])
		}
		findingLines = append(findingLines, line+" - "+note)
		if pyvalue.Truthy(item["anchor"]) {
			findingLines = append(findingLines, "    anchor: "+inline(item["anchor"]))
		}
	}
	return findingLines
}

// whatChanged is the WHAT CHANGED section.
func (x reviewIndex) whatChanged(receipt Obj) []string {
	review := x.review
	what := whatChangedLines(receipt)[:2]
	if len(review) > 0 {
		if kind := review["kind"]; pyvalue.Truthy(kind) {
			what[1] += "; the review judged " + pyvalue.Str(kind)
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
	return what
}

// fixScope is the FIX SCOPE section.
func (x reviewIndex) fixScope(report map[string]any) []string {
	source, findings := x.source, x.findings
	fix := fixScopeLines(source)
	if len(fix) > 2 {
		fix = fix[:len(fix)-1]
	}
	if len(findings) > 0 && pyvalue.Truthy(report["review"]) {
		fix[1] = strings.Replace(fix[1], "verified and unverified findings are out of scope", "verified, unverified and review-only findings are out of scope", 1)
	}
	return fix
}

// revisionScopeLines is the SCOPE section.
func revisionScopeLines(report map[string]any, receipt Obj) []string {
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
	return scope
}

// revisionProofLines is the PROOF section.
func revisionProofLines(report map[string]any) []string {
	proof := []string{"", "PROOF:"}
	if evidence := py.Items(report["evidence"]); len(evidence) > 0 {
		proof = append(proof, "  re-run what this review ran, and report command, exit code and result:")
		for _, raw := range evidence {
			if item, ok := raw.(string); ok {
				proof = append(proof, "    "+unheaded(item))
			} else {
				item := py.Dict(raw, false)
				proof = append(proof, "    "+unheaded(pyvalue.Str(item["check"])))
			}
		}
	} else {
		proof = append(proof, "  state the command, its exit code and what it showed, for each finding FIX SCOPE or REVERIFY AND RETURN names")
	}
	proof = append(proof, "  a passing string match is not a passing behaviour")
	return proof
}

// reverify is the REVERIFY AND RETURN section.
func (x reviewIndex) reverify(rid string, receipt Obj) []string {
	source := x.source
	reverify := reverifyLines(source)
	if len(correctionFindings(source)) > 0 {
		reverify[1] = strings.Replace(reverify[1], "re-check each finding FIX SCOPE names", "re-check each finding FIX SCOPE names (PROOF says how)", 1)
	}
	reverify = append(reverify, returnLines(rid, reportField(receipt, "executionGeneration"))...)
	return reverify
}

// verdictSections is the section that closes the message with the review's verdict, or
// nothing when the parent recorded no review. It refuses a verdict REVIEW-OUTPUT-01 does not know.
func (x reviewIndex) verdictSections(report map[string]any) []reportSection {
	review := x.review
	if !pyvalue.Truthy(report["review"]) {
		return nil
	}
	kind := py.Item(report["review"], "kind")
	line := "VERDICT: " + pyvalue.Str(kind)
	if !pyvalue.ItemEqual(kind, "PASS") && !pyvalue.ItemEqual(kind, "FAIL") && !pyvalue.ItemEqual(kind, "GO-WITH-FIXES") {
		panic(&py.PythonError{Class: "ValueError", Detail: pyvalue.Repr(kind) + " is not a review verdict; REVIEW-OUTPUT-01 fixes PASS, GO-WITH-FIXES, FAIL"})
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
		panic(&py.PythonError{Class: "ValueError", Detail: "a " + pyvalue.Str(kind) + " verdict carries no blocker count"})
	}
	return []reportSection{{"verdict", []string{"", line}, 0, 2, true, true}}
}
