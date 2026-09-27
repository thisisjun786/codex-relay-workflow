package supervisor

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/evidence"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Subset ported for todo 24; the reporting port owns and extends this.
const reportContractVersion = "0.2.28+codex.20260914090142"

var reportMeanings = map[string]string{
	"DONE":             "the child proved every recorded criterion against its own work",
	"NOOP":             "nothing needed doing, and the finding that established that is the deliverable",
	"BLOCKED":          "an external dependency is in the way",
	"UNSAFE":           "a human risk decision is required before this can proceed",
	"NEEDS_HUMAN":      "a judgment only the user can make",
	"BUDGET_EXHAUSTED": "a bound the plan actually stated ran out; best-so-far is adopted",
}
var reportOutcomes = map[string][]string{
	"DONE": {"ready_for_review"}, "NOOP": {"ready_for_review"},
	"BLOCKED": {"blocked_needs_input"}, "UNSAFE": {"blocked_needs_input"},
	"NEEDS_HUMAN": {"blocked_needs_input"}, "BUDGET_EXHAUSTED": {"interrupted", "failed"},
}
var nonVerification = map[string]string{
	"cxc_done":              "a DONE report is the child proving its own criteria, not the parent's verdict",
	"pull_request_opened":   "an open pull request is a place to review, not a review",
	"review_pass":           "a review PASS is one reviewer's judgment, not the parent's disposition",
	"required_checks_green": "a green required check is evidence for a verdict, not a verdict",
}

func reportRefusal(reason, detail string) error {
	return &store.RefusedError{Reason: reason, Detail: detail}
}
func checkReportStatus(status any, outcome string) error {
	text, isString := status.(string)
	allowed, ok := reportOutcomes[text]
	if !isString || !ok {
		return reportRefusal("outcome_inconsistent", fmt.Sprintf("%s is not a CXC report status this build maps. Accepted: DONE, NOOP, BLOCKED, UNSAFE, NEEDS_HUMAN, BUDGET_EXHAUSTED. Read against codexclaw %s; a newer contract needs the mapping extended rather than the value guessed at", evidence.Repr(status), reportContractVersion))
	}
	for _, v := range allowed {
		if v == outcome {
			return nil
		}
	}
	return reportRefusal("outcome_inconsistent", fmt.Sprintf("a %s report cannot accompany outcome '%s': %s means %s, which this contract pairs with %s", status, outcome, status, reportMeanings[text], strings.Join(allowed, ", ")))
}
func assertReportCurrent(head string, generation int64, wantedHead string, wantedGeneration int64) error {
	if generation != wantedGeneration {
		return reportRefusal("stale_generation", fmt.Sprintf("this report is generation %d and the assignment is on generation %d; it cannot answer for the current one", generation, wantedGeneration))
	}
	if wantedHead != "" && head != "" && head != wantedHead {
		return reportRefusal("stale_mark_context", fmt.Sprintf("this report is about head %s and the current head is %s; re-report against the head under review", head, wantedHead))
	}
	return nil
}
func reportRequired(value any, field string) (string, error) {
	text, ok := value.(string)
	if !ok {
		kind := evidence.TypeName(value)
		return "", reportRefusal("malformed_receipt", field+" is a line of text, not "+kind)
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return "", reportRefusal("malformed_receipt", "a work report states its "+field+"; a report the recipient cannot act on is the thing this record exists to replace")
	}
	return text, nil
}
func reportAssertReviewed(hasReview bool) error {
	if !hasReview {
		return fmt.Errorf("a verdict line states a review judgment; a progress or completion notice that renders one is dressing an update as a review")
	}
	return nil
}
func reportVerdictLine(kind any, blockers any) (string, error) {
	switch kind {
	case "PASS", "FAIL":
		if blockers != nil {
			return "", fmt.Errorf("a %s verdict carries no blocker count", kind)
		}
		return "VERDICT: " + evidence.Text(kind), nil
	case "GO-WITH-FIXES":
		count, ok := evidence.PyInt(blockers)
		if !ok || count < 1 {
			return "", fmt.Errorf("GO-WITH-FIXES states how many blockers it is going ahead with; a count below one is a PASS and should say so")
		}
		if count > 9999 {
			return "", fmt.Errorf("a blocker count of %d is past the point of being a review, and it sits on a line the message cannot shorten; the limit is 9999", count)
		}
		return fmt.Sprintf("VERDICT: GO-WITH-FIXES (blockers=%d)", count), nil
	}
	return "", fmt.Errorf("%s is not a review verdict; REVIEW-OUTPUT-01 fixes PASS, GO-WITH-FIXES, FAIL", evidence.Repr(kind))
}
func parseReportVerdict(line string) any {
	line = strings.TrimSpace(line)
	if line == "VERDICT: PASS" {
		return map[string]any{"kind": "PASS", "blockers": nil}
	}
	if line == "VERDICT: FAIL" {
		return map[string]any{"kind": "FAIL", "blockers": nil}
	}
	var count int
	if n, err := fmt.Sscanf(line, "VERDICT: GO-WITH-FIXES (blockers=%d)", &count); err == nil && n == 1 && count >= 1 && count <= 9999 && line == fmt.Sprintf("VERDICT: GO-WITH-FIXES (blockers=%d)", count) {
		return map[string]any{"kind": "GO-WITH-FIXES", "blockers": count}
	}
	return nil
}
func reportVersion(hasReport bool) string {
	if hasReport {
		return "relay-report/1"
	}
	return "relay-message/legacy"
}
func reportPRRef(repository string, number int64) string {
	if number == 0 {
		return ""
	}
	return fmt.Sprintf("%s#%d", repository, number)
}
