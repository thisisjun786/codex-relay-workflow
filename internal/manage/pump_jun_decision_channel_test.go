package manage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CRW-1390 (CRW-1314 D4, D1c's open item): on the 1.0 path a decision only Jun can make goes up through the supervisor channel
// (flow_jun_decision_request, CRW-1380), and the daemon delivers it to the parent above the project. The pump does not tell the
// management thread of a jun_decision the channel holds (its last delivery row says queued) or delivered (accepted): telling it again is a
// second notice of one decision. One the channel did not take - no delivery row (a legacy lead above the project, a staging refused, a
// store seat's report of a run with no project), held at a cap (refused) or sent with an unknown outcome (unknown) - is still the urgent
// jun_decision. A channel's jun_decision nobody acknowledged is read as any report is: report_unread once low_priority_seconds pass.
func TestPumpLeavesAJunDecisionTheSupervisorChannelHoldsOrDelivered(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	e, cfg, st, _ := pumpFlowTestSetup(t, &now, pumpFlowReportsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now.Add(-time.Hour), false)
	cases := []struct {
		id      string
		results []string
	}{
		{"rep_accepted", []string{"queued", "accepted"}},
		{"rep_queued", []string{"queued"}},
		{"rep_none", nil},
		{"rep_refused", []string{"queued", "refused"}},
		{"rep_unknown", []string{"queued", "unknown"}},
	}
	for _, c := range cases {
		flowTestRow(t, st, "run-1", "report", map[string]any{"report_id": c.id, "report_kind": "jun_decision_request", "subject": "decide " + c.id}, now.Add(-time.Minute))
		for _, result := range c.results {
			flowTestRow(t, st, "run-1", "report_delivery", map[string]any{"report_id": c.id, "result": result}, now.Add(-time.Minute))
		}
	}
	collect := func() map[string]string {
		res := pumpFlowReportsSource{}.Collect(context.Background(), e, cfg, &pumpState{Cursors: map[string]string{}}, pumpSettingsFrom(cfg))
		if res.Status != pumpSourceOK {
			t.Fatalf("the source is %q", res.Status)
		}
		out := map[string]string{}
		for _, ev := range res.Events {
			id := ev.ID[strings.LastIndex(ev.ID, ":")+1:]
			out[id] = ev.Kind
		}
		return out
	}
	got := collect()
	want := map[string]string{"rep_none": pumpKindJunDecision, "rep_refused": pumpKindJunDecision, "rep_unknown": pumpKindJunDecision}
	if len(got) != len(want) {
		t.Fatalf("the events are %v, want %v", got, want)
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("report %s raised %q, want %q (all: %v)", id, got[id], kind, got)
		}
	}
	// past low_priority_seconds, the channel's two unread decisions are unread reports like any other; the other three stay jun_decision
	now = now.Add(16 * time.Minute)
	got = collect()
	for _, id := range []string{"rep_accepted", "rep_queued"} {
		if got[id] != pumpKindReportUnread {
			t.Fatalf("report %s raised %q after low_priority_seconds, want %q (all: %v)", id, got[id], pumpKindReportUnread, got)
		}
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("report %s raised %q after low_priority_seconds, want %q (all: %v)", id, got[id], kind, got)
		}
	}
}
