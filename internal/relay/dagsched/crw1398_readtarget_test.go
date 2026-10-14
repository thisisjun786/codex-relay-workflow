package dagsched

import (
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1398 item 2: the periodic check (readTarget) attributes a moved-target fault to the newest batch plan on the integration ref, as the
// acknowledgement (ReceiveTargetNotice) does, not to the plan the target row names (the first one that recorded the target).
func TestCheckAttributesTheFaultToTheNewestBatchPlan(t *testing.T) {
	r := newReceiveKit(t)
	if target := r.recordTarget(t, "dev-int", "dev"); target.PlanID != "g" {
		t.Fatalf("the recorded target names plan %q; want g", target.PlanID)
	}
	r.putPlan("h", 0, "h-r1", addRelNode("b", dag.NodeImplementation))
	r.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)"+
		" VALUES ('stage-h-1', 'batch-h-1', 'h', 'intent', '', '', '', '', 0, '', ?, 'parent', '2099-01-01T00:00:00+00:00')", `{"integration_ref":"dev-int","base":"`+r.res.NewHead+`"}`)
	foreign := r.heads["a"]
	r.repo.git("push", "-q", r.remote, foreign+":refs/heads/dev")
	reading := oneReading(t, r.check(t))
	if reading.Status != TargetMovedOutside || reading.Moved == nil || reading.Moved.FaultID == "" {
		t.Fatalf("reading %+v; want a recorded moved-target fault", reading)
	}
	evidence := r.faultEvidence(t, reading.Moved.FaultID)
	if !strings.Contains(evidence, `"plan":"h"`) || strings.Contains(evidence, `"plan":"g"`) {
		t.Fatalf("fault evidence %s; want the newest batch plan h, not the target's first plan g", evidence)
	}
}
