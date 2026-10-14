package manage

import (
	"context"
	"strings"
	"testing"
	"time"
)

// CRW-1390, Argon V5 D3 (P1) of 118a891c: the daemon can recover a staged decision for Jun as held_uncertain (appending unknown) before
// the writer appends the queued row of its staging. unknown is a settled row like accepted and refused: the queued row after it does not
// hide it, so the pump reads unknown (report.SupersedesDelivery, as crw flow reports does) and still raises the urgent jun_decision. The
// readback's accepted after the unknown row supersedes it: that decision is the channel's and is not raised.
func TestPumpRaisesAJunDecisionWhoseUnknownSendAQueuedRowFollowed(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	e, cfg, st, _ := pumpFlowTestSetup(t, &now, pumpFlowReportsSource{})
	flowTestRun(t, st, "run-1", "orch-task", now.Add(-time.Hour), false)
	cases := []struct {
		id      string
		results []string
		last    string
	}{
		{"rep_unknown_then_queued", []string{"unknown", "queued"}, "unknown"},
		{"rep_queued_unknown_queued", []string{"queued", "unknown", "queued"}, "unknown"},
		{"rep_unknown_then_accepted", []string{"queued", "unknown", "accepted"}, "accepted"},
		{"rep_unknown_accepted_queued", []string{"unknown", "accepted", "queued"}, "accepted"},
	}
	for _, c := range cases {
		flowTestRow(t, st, "run-1", "report", map[string]any{"report_id": c.id, "report_kind": "jun_decision_request", "subject": "decide " + c.id}, now.Add(-time.Minute))
		for _, result := range c.results {
			flowTestRow(t, st, "run-1", "report_delivery", map[string]any{"report_id": c.id, "result": result}, now.Add(-time.Minute))
		}
	}
	data, err := pumpFlowRead(context.Background(), e, cfg)
	if err != nil {
		t.Fatal(err)
	}
	last := map[string]string{}
	for _, r := range data.Reports {
		last[r.ID] = r.LastDelivery
	}
	for _, c := range cases {
		if last[c.id] != c.last {
			t.Fatalf("report %s reads last delivery %q, want %q (all: %v)", c.id, last[c.id], c.last, last)
		}
	}
	res := pumpFlowReportsSource{}.Collect(context.Background(), e, cfg, &pumpState{Cursors: map[string]string{}}, pumpSettingsFrom(cfg))
	if res.Status != pumpSourceOK {
		t.Fatalf("the source is %q", res.Status)
	}
	got := map[string]string{}
	for _, ev := range res.Events {
		got[ev.ID[strings.LastIndex(ev.ID, ":")+1:]] = ev.Kind
	}
	want := map[string]string{"rep_unknown_then_queued": pumpKindJunDecision, "rep_queued_unknown_queued": pumpKindJunDecision}
	if len(got) != len(want) {
		t.Fatalf("the events are %v, want %v", got, want)
	}
	for id, kind := range want {
		if got[id] != kind {
			t.Fatalf("report %s raised %q, want %q (all: %v)", id, got[id], kind, got)
		}
	}
}
