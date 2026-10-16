package scenario

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

func planCycleWorld(t *testing.T) *Lab {
	t.Helper()
	lab := NewLab(t, fourNodes()...)
	lab.Cap = 1
	change := func(version string) {
		for _, node := range []string{"B", "C"} {
			lab.Relay.ChangeNode(node, func(spec *NodeSpec) {
				spec.Regions = []string{node + "-" + version + "/"}
			})
		}
	}
	lab.On("^design-audit:r1$", func(*Call) *fakehostPlan {
		change("one")
		return nil
	})
	lab.On("^build:A:executor$", func(*Call) *fakehostPlan {
		change("two")
		return nil
	})
	// Keep P2 until its signal has planned and reached the audit. The unit check and
	// signal binding run concurrently; changing back there could hide P2 from the signal.
	lab.On("^design-audit:replan-s2:r1$", func(*Call) *fakehostPlan {
		change("one")
		return nil
	})
	return lab
}

func checkPlanCycle(t *testing.T, lab *Lab) {
	t.Helper()
	if revision := releaseRevisions(lab)["B"]; revision != "r3" {
		t.Fatalf("B released under %s, want r3 after signalling and auditing the return to P1; calls=%v", revision, lab.Labels())
	}
	var reads []engine.Event
	for _, read := range lab.Rows("crw.planRead") {
		if flagOf(read.Data, "changed") {
			reads = append(reads, read)
		}
	}
	if len(reads) != 2 || str(reads[0].Data, "plan_digest") != str(reads[1].Data, "plan_digest") {
		t.Fatalf("want two changed reads of P1: %+v", reads)
	}
	first, returned := str(reads[0].Data, "occurrence"), str(reads[1].Data, "occurrence")
	if first == "" || returned == "" || first == returned {
		t.Fatalf("P1 occurrences must be distinct: %q, %q", first, returned)
	}
	trigger := "plan:" + returned + ":" + str(reads[1].Data, "plan_digest")
	var bound, audited, decided int
	for _, bind := range lab.Rows("crw.signalBind") {
		members, _ := bind.Data["members"].([]any)
		for _, member := range members {
			if str(member.(map[string]any), "call") == trigger {
				bound = bind.Seq
			}
		}
	}
	for _, audit := range lab.Rows("crw.designAudit") {
		if audit.Seq > bound {
			audited = audit.Seq
			break
		}
	}
	for _, done := range lab.Rows("crw.signalDone") {
		if str(done.Data, "revision") == "r3" {
			decided = done.Seq
		}
	}
	for _, release := range lab.Rows("crw.release") {
		if str(release.Data, "node") == "B" && flagOf(release.Data, "released") {
			if bound <= reads[1].Seq || audited <= bound || decided <= audited || release.Seq <= decided {
				t.Errorf("return to P1 must be read, signalled, audited and decided before B releases: read=%d bind=%d audit=%d done=%d release=%d", reads[1].Seq, bound, audited, decided, release.Seq)
			}
		}
	}
}

func TestVerificationPlanDigestReappearsBeforeRelease(t *testing.T) {
	lab := planCycleWorld(t)
	mustRun(t, lab)
	checkPlanCycle(t, lab)
}

func TestPlanDigestReappearsUnchangedResumeZeroLive(t *testing.T) {
	for _, stopAtAudit := range []bool{false, true} {
		name := "completed"
		if stopAtAudit {
			name = "return design audited before release"
		}
		t.Run(name, func(t *testing.T) {
			lab := planCycleWorld(t)
			var stopped atomic.Bool
			if stopAtAudit {
				lab.OnRow(func(event engine.Event) {
					if event.Op == "crw.designLedger" && event.Event == engine.EvCompleted && str(event.Data, "revision") == "r3" && stopped.CompareAndSwap(false, true) {
						lab.Kill()
					}
				})
				lab.Run()
				if !stopped.Load() || releaseRevisions(lab)["B"] != "" {
					t.Fatalf("did not stop after the return to P1 was audited before B's release: calls=%v releases=%v", lab.Labels(), releaseRevisions(lab))
				}
			} else {
				mustRun(t, lab)
				checkPlanCycle(t, lab)
			}
			before := lab.LastSeq()
			completed := map[string]bool{}
			for _, event := range lab.Events() {
				if event.Op == "agent" && event.Event == engine.EvCompleted {
					completed[event.Label] = true
				}
			}
			lab.NewProcess("proc-2")
			got := Value[laneResult](t, lab.Resume())
			if got.State != "closed" {
				t.Fatalf("resume: %+v; calls=%v", got, lab.Labels())
			}
			for _, call := range lab.LiveCalls(before) {
				if completed[call.Label] || strings.HasPrefix(call.Label, "replan:") {
					t.Errorf("unchanged resume ran %s live", call.Label)
				}
			}
			if !stopAtAudit && len(lab.LiveCalls(before)) != 0 {
				t.Errorf("completed unchanged resume made live calls: %+v", lab.LiveCalls(before))
			}
			for _, event := range rowsAfter(lab, before) {
				if event.Event == engine.EvSuperseded {
					t.Errorf("unchanged resume superseded %s at %s", event.Op, event.Path)
				}
				if !stopAtAudit && event.Kind == engine.KindCall && event.Event == engine.EvCompleted && strings.HasPrefix(event.Op, "crw.") && event.Op != "crw.classify" {
					t.Errorf("completed unchanged resume ran %s live", event.Op)
				}
			}
			checkPlanCycle(t, lab)
		})
	}
}

// signalAfterUnitCheck delays the second signal bind until the unit check finished: the red test's
// deterministic shape of "the unit check answered before the second signal read the plan"
// (CRW-1584).
type signalAfterUnitCheck struct {
	engine.Op
	checked <-chan struct{}
}

func (o signalAfterUnitCheck) Run(ctx context.Context, req engine.OpRequest) (engine.OpResult, error) {
	var args []json.RawMessage
	var k int
	if json.Unmarshal(req.Args, &args) == nil && len(args) > 1 {
		_ = json.Unmarshal(args[1], &k)
	}
	if k == 2 {
		select {
		case <-o.checked:
		case <-ctx.Done():
			return engine.OpResult{}, ctx.Err()
		}
	}
	return o.Op.Run(ctx, req)
}

// A unit check that finishes before the second signal bind reads the plan must still leave the
// intermediate plan judged: the signal sees P2 (CRW-1584).
func TestPlanCycleKeepsIntermediatePlanWhenUnitCheckFinishesFirst(t *testing.T) {
	lab := planCycleWorld(t)
	checked := make(chan struct{})
	var once sync.Once
	lab.OnSettled(func(row engine.Event) {
		if row.Op == "crw.unitCheck" {
			once.Do(func() { close(checked) })
		}
	})
	lab.OldOps = map[string]engine.Op{ops.NameSignalBind: signalAfterUnitCheck{Op: lab.lane.Ops()[ops.NameSignalBind], checked: checked}}
	mustRun(t, lab)
	checkPlanCycle(t, lab)
}
