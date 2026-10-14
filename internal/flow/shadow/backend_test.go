package shadow_test

import (
	"context"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/shadow"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/trace"
)

// CRW-1331: the shadow report names the backend each run of the trace records in its started row (a session run's subagent
// calls, or the App Server threads of a run that records none). The replay answers both from the trace: what it compares comes
// from the calls' prepared rows, which both backends write the same way.
func TestShadowReportNamesTheRecordedBackend(t *testing.T) {
	tr := &trace.Trace{Schema: trace.Schema, Runs: []trace.Run{
		{RunID: "run-s", Events: []trace.Event{{Seq: 1, Kind: engine.KindRun, Event: "started", Data: map[string]any{"backend": engine.BackendSession}}}},
		{RunID: "run-a", Events: []trace.Event{{Seq: 1, Kind: engine.KindRun, Event: "started", Data: map[string]any{}}}},
	}}
	rep, err := shadow.Compare(context.Background(), tr, shadow.Config{Script: []byte("export const meta = { name: 'f' }\nreturn 1\n"), Name: "f.js", Policy: engine.FakePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if got := []string{rep.Runs[0].Backend, rep.Runs[1].Backend}; got[0] != engine.BackendSession || got[1] != engine.BackendAppServer {
		t.Errorf("backends %v", got)
	}
}
