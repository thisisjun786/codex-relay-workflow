package supervisor

import (
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

func validateReportReview(v any) (any, error) {
	if v == nil {
		return nil, nil
	}
	review, ok := v.(map[string]any)
	if !ok {
		return nil, reportRefusal("malformed_receipt", fmt.Sprintf("a review is an object with a kind and its findings, not %s; a bare verdict word is not a review", reportType(v)))
	}
	rawFindings := review["findings"]
	items := []any{}
	if rawFindings != nil {
		var valid bool
		items, valid = rawFindings.([]any)
		if !valid {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("review findings is a list, not %s", reportType(rawFindings)))
		}
	}
	kind := review["kind"]
	_, err := reportVerdictLine(kind, review["blockers"])
	if err != nil {
		return nil, reportRefusal("malformed_receipt", err.Error())
	}
	findings := make([]any, 0, len(items))
	seen := map[string]bool{}
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("each review finding is an object naming a criterion, not %s", reportRepr(raw)))
		}
		id, ok := item["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, reportRefusal("malformed_receipt", "each review finding names a criterion id")
		}
		id = strings.TrimSpace(id)
		if seen[id] {
			return nil, reportRefusal("malformed_receipt", fmt.Sprintf("two findings name criterion %s; one criterion carries one finding, so the second would silently replace the first", store.PythonRepr(id)))
		}
		seen[id] = true
		if item["verdict"] != nil {
			verdict, ok := item["verdict"].(string)
			if !ok || (verdict != "verified" && verdict != "needs_changes" && verdict != "unverified") {
				return nil, reportRefusal("malformed_receipt", fmt.Sprintf("%s is not one of ('verified', 'needs_changes', 'unverified'); the criteria enum is frozen", reportRepr(item["verdict"])))
			}
		}
		finding := map[string]any{"id": id, "verdict": item["verdict"]}
		for _, field := range []string{"note", "anchor"} {
			text := ""
			if item[field] != nil {
				var valid bool
				text, valid = item[field].(string)
				if !valid {
					return nil, reportRefusal("malformed_receipt", fmt.Sprintf("a finding %s is a line of text, not %s", field, reportType(item[field])))
				}
			}
			text = strings.TrimSpace(text)
			text, err = reportLine(text, "a finding "+field, 1<<30)
			if err != nil {
				return nil, err
			}
			finding[field] = text
		}
		findings = append(findings, finding)
	}
	return map[string]any{"kind": kind, "blockers": review["blockers"], "findings": findings}, nil
}
