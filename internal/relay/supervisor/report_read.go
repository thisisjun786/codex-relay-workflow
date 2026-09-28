package supervisor

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// ReadWorkReports returns every submission in ascending order, including the handoff
// that belonged to that submission. Earlier submissions remain addressable after a correction.
func ReadWorkReports(ctx context.Context, s *store.Store, eventID string) ([]map[string]any, error) {
	rows, err := s.All(ctx, "SELECT * FROM work_reports WHERE event_id = ? ORDER BY submission_no", eventID)
	if err != nil {
		return nil, err
	}
	out := make([]map[string]any, 0, len(rows))
	if len(rows) == 0 {
		return out, nil
	}
	handoffs, err := s.All(ctx, "SELECT * FROM work_report_handoffs WHERE event_id = ?", eventID)
	if err != nil {
		return nil, err
	}
	byNumber := map[int64]map[string]any{}
	for _, row := range handoffs {
		h := map[string]any{"isDraft": row.Get("is_draft") == int64(1), "baseVerifiedAt": row.Get("base_verified_at")}
		for key, column := range map[string]string{"requiredDeclared": "required_declared", "checks": "checks", "reviewCoverage": "review_coverage", "threadDispositions": "thread_dispositions", "criterionEvidence": "criterion_evidence", "limitations": "limitations"} {
			h[key] = reportJSON(row.Get(column), []any{})
		}
		byNumber[row.Get("submission_no").(int64)] = h
	}
	for _, row := range rows {
		record := map[string]any{}
		for key, column := range map[string]string{
			"eventId": "event_id", "relationshipId": "relationship_id", "executionGeneration": "execution_generation", "revisionHash": "revision_hash", "submissionNo": "submission_no", "repository": "repository", "prNumber": "pr_number", "prUrl": "pr_url", "prState": "pr_state", "baseRef": "base_ref", "baseSha": "base_sha", "headSha": "head_sha", "criteriaDigest": "criteria_digest", "cxcStatus": "cxc_status", "cxcReason": "cxc_reason", "contractVersion": "contract_version", "summary": "summary", "nextAction": "next_action", "recordedAt": "recorded_at",
		} {
			record[key] = row.Get(column)
		}
		record["evidence"] = reportJSON(row.Get("evidence"), []any{})
		record["unresolved"] = reportJSON(row.Get("unresolved"), []any{})
		record["review"] = reportJSON(row.Get("review"), nil)
		record["restore"] = reportJSON(row.Get("restore"), map[string]any{})
		if h, ok := byNumber[row.Get("submission_no").(int64)]; ok {
			record["handoff"] = h
		}
		out = append(out, record)
	}
	return out, nil
}

// ReadWorkReport is the most recent submission, or nil for a legacy event.
func ReadWorkReport(ctx context.Context, s *store.Store, eventID string) (map[string]any, error) {
	all, err := ReadWorkReports(ctx, s, eventID)
	if err != nil || len(all) == 0 {
		return nil, err
	}
	return all[len(all)-1], nil
}

func reportJSON(value any, absent any) any {
	if value == nil || value == "" {
		return absent
	}
	var parsed any
	decoder := json.NewDecoder(strings.NewReader(value.(string)))
	decoder.UseNumber()
	if err := decoder.Decode(&parsed); err != nil {
		return absent
	}
	return parsed
}
