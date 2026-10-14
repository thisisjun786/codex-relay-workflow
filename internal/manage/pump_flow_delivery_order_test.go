package manage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CRW-1380, evaluation d3 of a49bcbbc: the daemon's settle can append the accepted report_delivery row before the writer appends the
// queued row of its staging. A queued row after an accepted one does not undo the acceptance: the unread report names accepted as its
// last delivery.
func TestUnreadReportLastDeliveryIsAcceptedWhenTheQueuedRowCameAfter(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, log := pumpFlowTestSetup(t, &now, pumpFlowReportsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now.Add(-3*time.Hour), false)
	flowTestRow(t, st, "run-1", "report", map[string]any{"report_id": "rep_a", "report_kind": "milestone_report", "milestone": "M1"}, now.Add(-30*time.Minute))
	flowTestRow(t, st, "run-1", "report_delivery", map[string]any{"report_id": "rep_a", "result": "accepted"}, now.Add(-29*time.Minute))
	flowTestRow(t, st, "run-1", "report_delivery", map[string]any{"report_id": "rep_a", "result": "queued"}, now.Add(-29*time.Minute))
	pumpFlowTestRound(t, e, cfg)
	now = now.Add(16 * time.Minute)
	pumpFlowTestRound(t, e, cfg)
	got := pumpFlowTestMessages(t, log)
	if len(got) != 1 || !strings.Contains(got[0], "report unread report=rep_a") || !strings.Contains(got[0], "last delivery=accepted") {
		t.Fatalf("messages %q, want the unread report with last delivery=accepted", got)
	}
}

// CRW-1380, Argon V5 F4 of 4d293b7b: the pump reads a report's last delivery as the flow side does (report.SupersedesDelivery): a
// queued row never supersedes a settled row (accepted or refused) that came before it, in either order of the two rows.
func TestPumpReadsASettledDeliveryOverAQueuedRowInEitherOrder(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	e, cfg, st, _ := pumpFlowTestSetup(t, &now, pumpFlowReportsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now.Add(-3*time.Hour), false)
	want := map[string]string{}
	for i, c := range []struct{ id, first, second, want string }{
		{"rep_refused_then_queued", "refused", "queued", "refused"},
		{"rep_queued_then_refused", "queued", "refused", "refused"},
		{"rep_accepted_then_queued", "accepted", "queued", "accepted"},
		{"rep_queued_then_accepted", "queued", "accepted", "accepted"},
	} {
		at := now.Add(-time.Duration(30-i) * time.Minute)
		flowTestRow(t, st, "run-1", "report", map[string]any{"report_id": c.id, "report_kind": "milestone_report", "milestone": "M1"}, at)
		flowTestRow(t, st, "run-1", "report_delivery", map[string]any{"report_id": c.id, "result": c.first}, at)
		flowTestRow(t, st, "run-1", "report_delivery", map[string]any{"report_id": c.id, "result": c.second}, at)
		want[c.id] = c.want
	}
	data, err := pumpFlowRead(context.Background(), e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, r := range data.Reports {
		got[r.ID] = r.LastDelivery
	}
	for id, result := range want {
		if got[id] != result {
			t.Fatalf("report %s reads last delivery %q, want %q (all: %v)", id, got[id], result, got)
		}
	}
}
