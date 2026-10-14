package dagsched

import (
	"context"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
)

// CRW-1395 1: a step of the lookup that has a candidate target stops the lookup there, observed or not. The plan's own target dev-int has no
// observation yet; another plan's target k-int shares the child's baseline and has an observation of a head outside its path. The comparison
// does not fall through to the refs found by the baseline and compare k-int: it compares nothing, records no fault, and the notice is the
// warning that the child's own target has no observation.
func TestReceiveStopsAtTheChildsOwnTargetWithoutAnObservation(t *testing.T) {
	r := newReceiveKit(t)
	r.seed("dispatch-request-1", r.base)
	r.putPlan("k", 0, "k-r1", addRelNode("c", dag.NodeImplementation))
	r.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)"+
		" VALUES ('stage-k-1', 'batch-k-1', 'k', 'intent', '', '', '', '', 0, '', ?, 'parent', '2026-10-14T00:00:00+00:00')", `{"integration_ref":"k-int","base":"`+r.base+`"}`)
	own := r.recordTarget(t, "dev-int", "dev") // the child's own plan's target, never observed
	if own.PlanID != "g" {
		t.Fatalf("the own target names plan %q; want g", own.PlanID)
	}
	other := r.recordTarget(t, "k-int", "k")
	if _, err := r.sched.ObserveTarget(context.Background(), other, r.heads["a"], "check", CheckActor); err != nil {
		t.Fatal(err)
	}
	notice := r.notice(t)
	if faults := targetFaults(t, r.batchKit); len(faults) != 0 {
		t.Fatalf("%d faults: the lookup fell through to another ref sharing the baseline (notice %v); want none", len(faults), notice)
	}
	if notice == nil || pyjson.Text(notice.Get("warning")) != TargetUnobservedWarning || notice.Get("faultId") != nil {
		t.Fatalf("notice %v; want the warning %s and no fault", notice, TargetUnobservedWarning)
	}
	targets, _ := notice.Get("targets").([]any)
	if len(targets) != 1 || !strings.Contains(pyjson.Text(notice.Get("detail")), "no observed head") {
		t.Fatalf("notice %v; want the one unobserved own target named", notice)
	}
	if pyjson.Text(notice.Get("baselineRevision")) != r.base {
		t.Fatalf("notice %v; want the baseline %s", notice, r.base)
	}
}

// CRW-1395 1: the same holds for the second step: a child of a later plan whose plan has a batch on dev-int, whose target has no observation, is
// not compared with another ref that shares its baseline and has one.
func TestReceiveStopsAtTheBatchRefsTargetWithoutAnObservation(t *testing.T) {
	r := newReceiveKit(t)
	r.recordTarget(t, "dev-int", "dev")
	child := r.laterPlanChild(t)
	r.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)"+
		" VALUES ('stage-h-1', 'batch-h-1', 'h', 'intent', '', '', '', '', 0, '', ?, 'parent', '2026-10-14T00:00:00+00:00')", `{"integration_ref":"dev-int","base":"`+r.base+`"}`)
	child.seed("dispatch-request-later", r.base)
	r.putPlan("k", 0, "k-r1", addRelNode("c", dag.NodeImplementation))
	r.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)"+
		" VALUES ('stage-k-1', 'batch-k-1', 'k', 'intent', '', '', '', '', 0, '', ?, 'parent', '2026-10-14T00:00:01+00:00')", `{"integration_ref":"k-int","base":"`+r.base+`"}`)
	other := r.recordTarget(t, "k-int", "k")
	if _, err := r.sched.ObserveTarget(context.Background(), other, r.heads["a"], "check", CheckActor); err != nil {
		t.Fatal(err)
	}
	notice := child.notice(t)
	if faults := targetFaults(t, r.batchKit); len(faults) != 0 {
		t.Fatalf("%d faults: the lookup fell through past the batch ref's target (notice %v); want none", len(faults), notice)
	}
	if notice == nil || pyjson.Text(notice.Get("warning")) != TargetUnobservedWarning {
		t.Fatalf("notice %v; want the warning %s", notice, TargetUnobservedWarning)
	}
}

// CRW-1395 3: a fault found at the acknowledgement of a later plan's child is attributed to the newest plan that recorded a batch on the integration
// ref, not to the plan the target row names (the first one that recorded it).
func TestReceiveAttributesTheFaultToTheNewestBatchPlan(t *testing.T) {
	r := newReceiveKit(t)
	if target := r.recordTarget(t, "dev-int", "dev"); target.PlanID != "g" {
		t.Fatalf("the recorded target names plan %q; want g", target.PlanID)
	}
	child := r.laterPlanChild(t)
	r.exec("INSERT INTO dag_integration_stages (stage_id, batch_id, plan_id, stage, node_id, acceptance_id, event_id, revision_hash, generation, head_sha, detail, recorded_by, recorded_at)"+
		" VALUES ('stage-h-1', 'batch-h-1', 'h', 'intent', '', '', '', '', 0, '', ?, 'parent', '2099-01-01T00:00:00+00:00')", `{"integration_ref":"dev-int","base":"`+r.res.NewHead+`"}`)
	child.seed("dispatch-request-later", r.heads["a"])
	child.observe(t, r.elsewhere())
	notice := child.notice(t)
	id := pyjson.Text(notice.Get("faultId"))
	if notice == nil || id == "" {
		t.Fatalf("notice %v; want a fault", notice)
	}
	evidence := r.faultEvidence(t, id)
	if !strings.Contains(evidence, `"plan":"h"`) {
		t.Fatalf("fault evidence %s; want the newest batch plan h, not the target's first plan g", evidence)
	}
}
