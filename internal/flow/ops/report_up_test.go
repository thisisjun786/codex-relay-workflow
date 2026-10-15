package ops

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

// recordingReporter keeps the reports crw.reportUp hands it.
type recordingReporter struct {
	mu      sync.Mutex
	reports []Report
}

func (r *recordingReporter) ReportUp(_ context.Context, rep Report) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reports = append(r.reports, rep)
	return "rep_" + rep.Milestone, nil
}

// TestReportUpHandsTheCrosscheckAndReflectionToTheReporter (F-14): the close argument of crw.reportUp carries the crosscheck and the
// reflection of the close, and the op hands both to the Reporter with the gate, the final review and the audit.
func TestReportUpHandsTheCrosscheckAndReflectionToTheReporter(t *testing.T) {
	w := newWorld(t)
	rep := &recordingReporter{}
	w.lane = NewLane(Deps{RunID: "run-1", Journal: w.j, Relay: w.relay, Decisions: &MemoryDecisions{}, Actor: "parent", Reporter: rep,
		Now: func() time.Time { return time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC) }})
	v := json.RawMessage(`{"verdict":"verified","guide_rev":"crw-crosscheck/1","milestone":"M1","head":"t1","findings":[],"carried":[]}`)
	gate := json.RawMessage(`{"head":"t1","state":"pass"}`)
	audit := json.RawMessage(`{"summary":{"ok":1}}`)
	w.must(NameReportUp, "M1", map[string]any{"base": "b1", "tip": "t1", "gate": gate, "final": nil, "audit": audit, "crosscheck": v, "reflection": "ALIGNED"})
	if len(rep.reports) != 1 {
		t.Fatalf("%d reports", len(rep.reports))
	}
	got := rep.reports[0].Close
	if !jsonEqual(t, got.Crosscheck, v) || got.Reflection != "ALIGNED" {
		t.Errorf("the reporter got crosscheck %s reflection %q", got.Crosscheck, got.Reflection)
	}
	if !jsonEqual(t, got.Gate, gate) || !jsonEqual(t, got.Audit, audit) || rep.reports[0].Range != (CloseRange{Base: "b1", Tip: "t1"}) {
		t.Errorf("the reporter got %+v", rep.reports[0])
	}
	if len(got.Final) != 0 && string(got.Final) != "null" {
		t.Errorf("final %s", got.Final)
	}
}
