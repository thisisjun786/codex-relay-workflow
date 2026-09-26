// Subset ported for todo 26 (report.py current_reports, the heads the merge gate reads, and
// required_for_candidate, which the merge-turn grant notice proposes); todo 24 owns and
// extends this.

package supervisor

import (
	"context"
	"encoding/json"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CurrentReportHeads is the head_sha of every row report.current_reports(db, relationship)
// returns: for the newest generation with a head-bearing report, each event's own latest
// head-bearing submission. ctx carries the caller's open transaction, if any.
func CurrentReportHeads(ctx context.Context, s *store.Store, relationship string) ([]string, error) {
	newest, err := s.One(ctx, "SELECT MAX(execution_generation) AS generation FROM work_reports"+
		"  WHERE relationship_id = ? AND head_sha IS NOT NULL", relationship)
	if err != nil || newest == nil || newest.Get("generation") == nil {
		return nil, err
	}
	rows, err := s.All(ctx, "SELECT w.event_id, w.submission_no, w.head_sha, w.repository, w.base_ref,"+
		"       h.required_declared"+
		"  FROM work_reports w LEFT JOIN work_report_handoffs h"+
		"    ON h.event_id = w.event_id AND h.submission_no = w.submission_no"+
		" WHERE w.relationship_id = ? AND w.execution_generation = ? AND w.head_sha IS NOT NULL"+
		"   AND w.submission_no = (SELECT MAX(l.submission_no) FROM work_reports l"+
		"                           WHERE l.event_id = w.event_id AND l.head_sha IS NOT NULL)"+
		" ORDER BY w.event_id", relationship, newest.Get("generation"))
	if err != nil {
		return nil, err
	}
	heads := make([]string, 0, len(rows))
	for _, row := range rows {
		head, _ := row.Get("head_sha").(string)
		heads = append(heads, head)
	}
	return heads, nil
}

// RequiredReading is report.required_for_candidate's answer: Required nil for Python's None
// (with Reason), otherwise the names the reading found and where it was recorded.
type RequiredReading struct {
	Required     []string
	Known        bool
	Reason       string
	EventID      string
	SubmissionNo any
}

// RequiredForCandidate is report.required_for_candidate: what the candidate's own
// merge-readiness reading found the target requires, or why nothing.
func RequiredForCandidate(ctx context.Context, s *store.Store, relationship, repository, baseRef, head string) (RequiredReading, error) {
	absent := func(reason string) (RequiredReading, error) { return RequiredReading{Reason: reason}, nil }
	if relationship == "" {
		return absent("the turn names no assignment")
	}
	if head == "" {
		return absent("the grant names no candidate head")
	}
	newest, err := s.One(ctx, "SELECT MAX(execution_generation) AS generation FROM work_reports"+
		"  WHERE relationship_id = ? AND head_sha IS NOT NULL", relationship)
	if err != nil {
		return RequiredReading{}, err
	}
	if newest == nil || newest.Get("generation") == nil {
		return absent("no work report on this assignment names a head")
	}
	rows, err := s.All(ctx, "SELECT w.event_id, w.submission_no, w.head_sha, w.repository, w.base_ref,"+
		"       h.required_declared"+
		"  FROM work_reports w LEFT JOIN work_report_handoffs h"+
		"    ON h.event_id = w.event_id AND h.submission_no = w.submission_no"+
		" WHERE w.relationship_id = ? AND w.execution_generation = ? AND w.head_sha IS NOT NULL"+
		"   AND w.submission_no = (SELECT MAX(l.submission_no) FROM work_reports l"+
		"                           WHERE l.event_id = w.event_id AND l.head_sha IS NOT NULL)"+
		" ORDER BY w.event_id", relationship, newest.Get("generation"))
	if err != nil {
		return RequiredReading{}, err
	}
	var heads []string
	seen := map[string]bool{}
	for _, row := range rows {
		h, _ := row.Get("head_sha").(string)
		if !seen[h] {
			seen[h] = true
			heads = append(heads, h)
		}
	}
	sort.Strings(heads)
	if len(heads) != 1 || heads[0] != head {
		quoted := make([]string, len(heads))
		for i, h := range heads {
			quoted[i] = StrRepr(h)
		}
		return absent("the current work report is about " + strings.Join(quoted, ", ") + ", not the candidate " + StrRepr(head))
	}
	for _, row := range rows {
		if row.Get("repository") != any(repository) {
			return absent("the current work report is about another repository than " + repository)
		}
	}
	for _, row := range rows {
		if row.Get("base_ref") == nil {
			return absent("the current work report records no base, so its reading cannot be tied to " + baseRef)
		}
	}
	for _, row := range rows {
		if row.Get("base_ref") != any(baseRef) {
			return absent("the current work report is about another base than " + baseRef)
		}
	}
	for _, row := range rows {
		if row.Get("required_declared") == nil {
			return absent("a current work report records no merge-readiness handoff")
		}
	}
	var readings [][]string
	for _, row := range rows {
		var names []any
		text, _ := row.Get("required_declared").(string)
		ok := json.Unmarshal([]byte(text), &names) == nil && names != nil
		set := map[string]bool{}
		for _, one := range names {
			name, isStr := one.(string)
			ok = ok && isStr
			set[name] = true
		}
		if !ok {
			event, _ := row.Get("event_id").(string)
			return absent("the recorded required set of work report " + event + " is unreadable")
		}
		sorted := make([]string, 0, len(set))
		for name := range set {
			sorted = append(sorted, name)
		}
		sort.Strings(sorted)
		readings = append(readings, sorted)
	}
	for _, r := range readings[1:] {
		if strings.Join(r, "\x00") != strings.Join(readings[0], "\x00") || len(r) != len(readings[0]) {
			return absent("the current work reports' readings disagree about what is required")
		}
	}
	event, _ := rows[0].Get("event_id").(string)
	return RequiredReading{Required: readings[0], Known: true, EventID: event, SubmissionNo: rows[0].Get("submission_no")}, nil
}
