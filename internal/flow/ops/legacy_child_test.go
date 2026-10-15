package ops

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

// The issue-child path is retired (CRW-1415): the lane installs no crw.releaseChild operation, so no script can start a child. The op
// name stays an old run's recording call that is read only: a journal whose completed crw.releaseChild row an old run wrote is still
// a recorded call, and a resume does not send it again.
func TestLaneOperationsHaveNoReleaseChild(t *testing.T) {
	if _, ok := NewLane(Deps{}).Ops()["releaseChild"]; ok {
		t.Errorf("the lane still installs crw.releaseChild")
	}
	if OpReleaseChild != "crw.releaseChild" {
		t.Errorf("OpReleaseChild is %q, want the old row's op name crw.releaseChild", OpReleaseChild)
	}
	if !IsRecordOp(OpReleaseChild) {
		t.Errorf("an old crw.releaseChild row is no longer read as a recording call")
	}
	events := []engine.Event{{Seq: 1, RunID: "run-old", Kind: engine.KindCall, CallID: "c_child", Op: OpReleaseChild, Event: engine.EvCompleted,
		Data: map[string]any{"node": "A", "child": "child-A", "started": true}}}
	d := DecideRecord(events, "run-old", "c_child")
	if !d.Recorded || d.Resend || d.Row == nil || d.Row.Op != OpReleaseChild {
		t.Errorf("an old completed crw.releaseChild row: %+v, want recorded and not sent again", d)
	}
}
