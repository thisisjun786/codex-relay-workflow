package adapter

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/daemon"
)

// CRW-1390, Argon V5 D3 (P1) of 118a891c: a daemon claims the staged decision for Jun and dies after the transport started; the next
// daemon recovers the stranded send as held_uncertain and appends unknown - all before the writer appends the queued row of its staging.
// unknown is a settled row: the queued row after it does not hide it. The report reads unknown and standing for the writer's answer, crw
// flow reports and a replay, which stages nothing and writes nothing, and the send is not repeated.
func TestFlowReportE2EAnUncertainSendBeforeTheQueuedRowReadsUnknown(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.open(mgmtA)
	f := w.flowRun("P-A")
	decision := report.UpRequest{Kind: report.KindJunDecisionRequest, Run: flowRun, Plan: flowPlan, Subject: "ship before the review?", Body: []byte("the evidence")}
	wr := f.writer()
	wr.Rows = settlingAppender{Appender: wr.Rows, before: func() {
		dyingTick(t, w)
		w.clock.T += 400
		w.d = daemon.New(w.s, w.a, w.clock, w.c)
		w.tick(3)
	}}
	res, err := wr.Up(w.ctx, decision)
	if err != nil {
		t.Fatal(err)
	}
	if _, state := f.flowMessage("flow_jun_decision_request"); state != "held_uncertain" {
		t.Fatalf("the stranded message is %s\n%s", state, w.dump())
	}
	if got := f.deliveryOrder(res.ReportID); len(got) != 2 || got[0] != "unknown" || got[1] != "queued" {
		t.Fatalf("the delivery rows are %v, want the recovery's unknown before the writer's queued\n%s", got, w.dump())
	}
	if res.Delivery != "unknown" || !res.Standing {
		t.Fatalf("the report-up answered %+v, want unknown and standing", res)
	}
	views, err := report.ReportsMatching(w.ctx, w.s.DB, report.ReportFilter{Project: "P-A"})
	if err != nil || len(views) != 1 || !views[0].Standing || views[0].LastDelivery != "unknown" {
		t.Fatalf("the report reads %+v (%v), want unknown and standing", views, err)
	}
	again, err := f.writer().Up(w.ctx, decision)
	if err != nil || !again.Replayed || again.Delivery != "unknown" || !again.Standing {
		t.Fatalf("the replay answered %+v (%v), want unknown and standing", again, err)
	}
	w.tick(2)
	if got := f.deliveryOrder(res.ReportID); len(got) != 2 {
		t.Fatalf("after the replay the delivery rows are %v, want them unchanged", got)
	}
	if len(w.turnsOf(mgmtA)) != 0 {
		t.Fatalf("the uncertain send was repeated: %q", w.turnsOf(mgmtA))
	}
	f.noParentQueue()
}
