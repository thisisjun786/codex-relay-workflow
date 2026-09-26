package mergeturn

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/supervisor"
)

func reportForGrant(t *testing.T, w *fx, relationship, event, head string, submission int, required *string) {
	t.Helper()
	_, err := w.s.Querier(w.ctx).ExecContext(w.ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, event, submission, relationship, 3, "rev", fxRepo, fxBase, head, "done", "r", "1", "s", "n", fxISO)
	if err != nil {
		t.Fatal(err)
	}
	if required != nil {
		_, err = w.s.Querier(w.ctx).ExecContext(w.ctx, `INSERT INTO work_report_handoffs (event_id,submission_no,is_draft,required_declared,checks,review_coverage,thread_dispositions,recorded_at) VALUES (?,?,?,?,?,?,?,?)`, event, submission, 0, *required, "[]", "{}", "[]", fxISO)
		if err != nil {
			t.Fatal(err)
		}
	}
}
func Test26_MTW_10_gate_reads_every_current_event_not_global_submission(t *testing.T) {
	w, _, r := queueFixture(t)
	reportForGrant(t, w, r, "event-a", "head-old", 1, nil)
	reportForGrant(t, w, r, "event-a", "head-a", 2, nil)
	reportForGrant(t, w, r, "event-b", "head-b", 1, nil)
	heads, err := supervisor.CurrentReportHeads(w.ctx, w.s, r)
	if err != nil || len(heads) != 2 || heads[0] != "head-a" || heads[1] != "head-b" {
		t.Fatal(heads, err)
	}
	turn := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-a", true, ClaimOptions{Relationship: sql.NullString{String: r, Valid: true}}))["turnId"].(string)
	w.answer(turn, alpha.TaskID)
	_, err = w.check(turn, "head-a", "base-0", "")
	if reasonOf(err) != "revision_ambiguous" {
		t.Fatal(err)
	}
	_, err = w.s.Querier(w.ctx).ExecContext(w.ctx, "DELETE FROM work_reports WHERE event_id='event-b'")
	if err != nil {
		t.Fatal(err)
	}
	_, err = w.check(turn, "head-old", "base-0", "")
	if reasonOf(err) != "merge_candidate_moved" {
		t.Fatal(err)
	}
	// A later report without a head does not replace this event's latest
	// head-bearing submission; the gate still uses head-a.
	_, err = w.s.Querier(w.ctx).ExecContext(w.ctx, `INSERT INTO work_reports (event_id,submission_no,relationship_id,execution_generation,revision_hash,repository,base_ref,head_sha,cxc_status,cxc_reason,contract_version,summary,next_action,recorded_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, "event-a", 3, r, 3, "rev", fxRepo, fxBase, nil, "done", "r", "1", "s", "n", fxISO)
	if err != nil {
		t.Fatal(err)
	}
	heads, err = supervisor.CurrentReportHeads(w.ctx, w.s, r)
	if err != nil || len(heads) != 1 || heads[0] != "head-a" {
		t.Fatal(heads, err)
	}
	// A separate turn granted for the historical head reaches the work-report gate.
	w.must(w.m.Release(w.ctx, turn, alpha.TaskID, "returned", "try historical head", ""))
	historical := w.must(w.m.Request(w.ctx, fxRepo, fxBase, fxA, alpha.TaskID, alpha.HostID, "head-old", true, ClaimOptions{Relationship: sql.NullString{String: r, Valid: true}}))["turnId"].(string)
	w.answer(historical, alpha.TaskID)
	_, err = w.check(historical, "head-old", "base-0", "")
	if reasonOf(err) != "merge_candidate_moved" || !strings.Contains(err.Error(), "names head 'head-a'") {
		t.Fatal(err)
	}
}
