package manage

import (
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
