package adapter

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
)

// CRW-1380, evaluation d2 and d3 of a49bcbbc: the delivery rows of a report over the supervisor channel when the settle is not the
// tidy one after the writer returned. A management thread busy until the busy cap holds the message before any transport call, and
// that hold is a refused delivery row as a cap the settle holds is; a settle that lands between the staging and the writer's queued row
// leaves the report read as accepted, not as queued after it.

// settlingAppender runs before just ahead of the writer's delivery row: the daemon settles between the staging and the queued row.
type settlingAppender struct {
	report.Appender
	before func()
}

func (a settlingAppender) Append(ctx context.Context, row report.NewRow) (int64, error) {
	if row.Kind == report.KindDelivery && a.before != nil {
		a.before()
	}
	return a.Appender.Append(ctx, row)
}

// deliveryOrder is the results of the report_delivery rows of a report, in the order they were written.
func (f *flowWorld) deliveryOrder(id string) []string {
	f.t.Helper()
	rows, err := report.ReadRows(f.ctx, f.s.DB, "", report.KindDelivery)
	if err != nil {
		f.t.Fatal(err)
	}
	var out []string
	for _, r := range rows {
		if r.Str("report_id") == id {
			out = append(out, r.Str("result"))
		}
	}
	return out
}

func blockedReport() report.UpRequest {
	return report.UpRequest{Kind: report.KindProjectBlocked, Run: flowRun, Plan: flowPlan, Subject: "the gate is red", Body: []byte("the gate is red on the milestone")}
}

// TestFlowReportE2EASettleBeforeTheQueuedRowReadsAccepted: the daemon claims, sends and settles the report's message before the writer
// appends its queued row, so the accepted row comes first. The report reads accepted (not standing) for crw flow reports and for a
// replay, which stages nothing and writes nothing.
func TestFlowReportE2EASettleBeforeTheQueuedRowReadsAccepted(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.open(mgmtA)
	f := w.flowRun("P-A")
	wr := f.writer()
	wr.Rows = settlingAppender{Appender: wr.Rows, before: func() { w.tick(3) }}
	res, err := wr.Up(w.ctx, blockedReport())
	if err != nil {
		t.Fatal(err)
	}
	if got := f.deliveryOrder(res.ReportID); len(got) != 2 || got[0] != "accepted" || got[1] != "queued" {
		t.Fatalf("the delivery rows are %v, want the settle's accepted before the writer's queued\n%s", got, w.dump())
	}
	if len(w.turnsOf(mgmtA)) != 1 {
		t.Fatalf("management A's turns %q", w.turnsOf(mgmtA))
	}
	views, err := report.ReportsMatching(w.ctx, w.s.DB, report.ReportFilter{Project: "P-A"})
	if err != nil || len(views) != 1 || views[0].Standing || views[0].LastDelivery != "accepted" {
		t.Fatalf("the report reads %+v (%v), want accepted and not standing", views, err)
	}
	again, err := f.writer().Up(w.ctx, blockedReport())
	if err != nil || !again.Replayed || again.Delivery != "accepted" || again.Standing {
		t.Fatalf("the replay answered %+v (%v), want accepted and not standing", again, err)
	}
	w.tick(2)
	if got := f.deliveryOrder(res.ReportID); len(got) != 2 || len(w.turnsOf(mgmtA)) != 1 {
		t.Fatalf("after the replay the delivery rows are %v and the turns %d", got, len(w.turnsOf(mgmtA)))
	}
	f.noParentQueue()
}

// TestFlowReportE2EBusyCapHoldWritesTheRefusedDelivery: the management thread stays busy; each tick defers the message before any
// transport call, and the deferral that reaches the busy cap holds it. That hold writes one refused report_delivery row, in the
// deferral's transaction; more ticks write no second.
func TestFlowReportE2EBusyCapHoldWritesTheRefusedDelivery(t *testing.T) {
	w := newMgmtWorld(t)
	w.relay("linkage-bind", "--role", "supervisor", "--scope-kind", "management", "--scope", mgmtA, "--task", mgmtA, "--host", "host")
	w.bindOwner("P-A", ownerA, mgmtA)
	w.host.Handle("thread/read", func(json.RawMessage) fakehost.Reply {
		return fakehost.Reply{Result: map[string]any{"thread": map[string]any{"status": map[string]any{"type": "active", "activeFlags": []any{}}, "canAcceptDirectInput": true}}}
	})
	w.open(mgmtA)
	f := w.flowRun("P-A")
	res, err := f.writer().Up(w.ctx, blockedReport())
	if err != nil {
		t.Fatal(err)
	}
	message, _ := f.flowMessage("flow_project_blocked")
	held := false
	for i := 0; i < 200 && !held; i++ {
		w.tick(1)
		w.clock.T += 301
		row, err := w.s.SupervisorMessage(w.ctx, message)
		if err != nil {
			t.Fatal(err)
		}
		held = row.HoldReason.Valid && row.HoldReason.String == "busy_cap"
	}
	if !held {
		t.Fatalf("the message never reached the busy cap\n%s", w.dump())
	}
	if len(w.turnsOf(mgmtA)) != 0 {
		t.Fatalf("a busy thread was given a turn: %q", w.turnsOf(mgmtA))
	}
	if got := f.deliveries(res.ReportID); got["queued"] != 1 || got["refused"] != 1 || len(got) != 2 {
		t.Fatalf("after the busy cap the deliveries are %v, want one queued and one refused\n%s", got, w.dump())
	}
	rows, err := report.ReadRows(w.ctx, w.s.DB, flowRun, report.KindDelivery)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Str("result") == "refused" && r.Str("reason") != "busy_cap" {
			t.Fatalf("the refused row names %q, want busy_cap: %+v", r.Str("reason"), r.Payload)
		}
	}
	w.tick(3)
	if got := f.deliveries(res.ReportID); got["refused"] != 1 {
		t.Fatalf("more ticks over the hold wrote the deliveries %v", got)
	}
	f.noParentQueue()
}
