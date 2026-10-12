package cli

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/compile"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/journal"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/report"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// CRW-1291: a plan revision the relay's head took after the revision a run pinned is not followed by the run. The relay refuses the
// release of a node whose slice it changed, and the run holds (plan_superseded) and tells its parent.

// retitle is a plan revision an orchestrator puts on the relay during a run: it retitles an issue, so the node's slice at the plan's
// head changes and the revision the run pinned does not.
func (l *laneLab) retitle(r *SchedRelay, key string) {
	l.t.Helper()
	in, err := DecodeHandoff(l.handoffBytes(func(in *compile.Input) {
		for i := range in.Projects[0].Issues {
			if in.Projects[0].Issues[i].Key == key {
				in.Projects[0].Issues[i].Title = "issue " + key + ", retitled by a plan revision"
			}
		}
	}))
	if err != nil {
		l.t.Fatal(err)
	}
	// the relay's own store: the lab's handle is closed while a run process holds the store
	repo := &dag.Repo{Store: r.Sched.Store}
	head, _, err := repo.Snapshot(context.Background(), l.plan, 0)
	if err != nil {
		l.t.Fatal(err)
	}
	res, err := compile.Compile(in, compile.Options{Bases: map[string]dag.State{l.plan: dag.StateFromSnapshot(head)}})
	if err != nil || res.Plans[0].Change == nil {
		l.t.Fatalf("the retitle compiled to no plan revision: %v", err)
	}
	change := *res.Plans[0].Change
	change.CoordinatorEpoch = r.Sched.ExpectedEpoch
	if _, err := repo.Put(context.Background(), change); err != nil {
		l.t.Fatal(err)
	}
}

// parentRecorder stands in for `crw manage send-parent`: it records what the run tells its parent.
type parentRecorder struct {
	mu   sync.Mutex
	sent []report.ParentSend
}

func (p *parentRecorder) SendSupervisor(context.Context, report.SupervisorSend) (report.SendResult, error) {
	return report.SendResult{Class: "accepted"}, nil
}

func (p *parentRecorder) SendParent(_ context.Context, m report.ParentSend) (report.SendResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.sent = append(p.sent, m)
	return report.SendResult{Class: "queued", RequestID: "req-" + m.LogicalID}, nil
}

// TestRelayPlanChangedRaisesRunHoldAndReportsUp: the run pinned revision 1; after A integrated, an orchestrator put revision 2, which
// gives B another slice. The relay refuses B's release, and the run does not follow the new revision: it raises the hold plan_superseded
// (an escalation of B that asks the parent), releases nothing more, and tells the parent the revision it pinned, the relay's head
// revision, the blocked node and the actions (end or cancel the run and start a new one on the new revision, or close the node by
// escalation). A resume does not restart anything: it asks the relay nothing and the run still holds.
func TestRelayPlanChangedRaisesRunHoldAndReportsUp(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "B-1", Milestone: "M1", After: []string{"A-1"}})
	a, b := l.nodeOf["A-1"], l.nodeOf["B-1"]
	parent := &parentRecorder{}
	prev := journal.UpwardFactory
	t.Cleanup(func() { journal.UpwardFactory = prev })
	journal.UpwardFactory = func(st *store.Store, j *journal.Journal, runID, _, _ string) engine.UpwardHook {
		return &report.RunHook{Store: st, Journal: j, Rows: report.JournalAppender{Journal: j, RunID: runID, Store: st}, Sender: parent, Run: runID}
	}
	moved := false
	l.after = func(r *SchedRelay, effect string) {
		if effect == "integrate:"+a && !moved {
			moved = true
			l.retitle(r, "B-1")
		}
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	held := func(res Result) {
		t.Helper()
		var v laneValue
		if res.Status != engine.RunCompleted || json.Unmarshal(res.Value, &v) != nil || v.State != "held" {
			t.Fatalf("the run: %s %s %s", res.Status, res.Error, res.Value)
		}
		for _, ln := range v.Lanes {
			if ln.Node == b && (ln.State != "held" || ln.Reason != ops.ReasonPlanSuperseded) {
				t.Errorf("lane B: %+v", ln)
			}
		}
	}
	held(l.runToEnd(spec.RunID))
	if !moved {
		t.Fatal("A never integrated")
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions WHERE node_id = ?", b); n != 0 {
		t.Errorf("B has %d executions", n)
	}
	var hold []engine.Event
	for _, e := range l.events() {
		if e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && e.Data["reason"] == ops.ReasonPlanSuperseded {
			hold = append(hold, e)
		}
	}
	if len(hold) != 1 || hold[0].Data["node"] != b || hold[0].Data["ask"] != ops.ReasonPlanSuperseded {
		t.Fatalf("the holds: %+v", hold)
	}
	evidence, _ := hold[0].Data["evidence"].(map[string]any)
	if evidence["pinned_revision"] != float64(1) || evidence["head_revision"] != float64(2) || evidence["node"] != b {
		t.Errorf("the hold records %v", evidence)
	}
	id, _ := hold[0].Data["escalation_id"].(string)
	var told []report.ParentSend
	for _, m := range parent.sent {
		if m.LogicalID == "esc-"+id {
			told = append(told, m)
		}
	}
	if len(told) != 1 || told[0].Thread != labOwner {
		t.Fatalf("the parent was told %+v", parent.sent)
	}
	for _, want := range []string{"reason=plan_superseded", "node=" + b, "pinned revision 1", "head revision 2", "crw flow cancel " + spec.RunID, "start a new run on revision 2", "close node " + b + " by escalation", "nothing restarts on its own"} {
		if !strings.Contains(told[0].Text, want) {
			t.Errorf("the notice does not say %q:\n%s", want, told[0].Text)
		}
	}
	releases := func() (n int) {
		for _, e := range l.effects {
			if e == "release:"+b {
				n++
			}
		}
		return n
	}
	starts := func() (n int) {
		for _, e := range l.events() {
			if e.Kind == engine.KindRun && e.Event == "started" {
				n++
			}
		}
		return n
	}
	asked, started := releases(), starts()
	if code, _ := l.process(spec.RunID, true); code != 0 || starts() != started+1 {
		t.Fatalf("the resume exited %d and started %d processes", code, starts()-started)
	}
	if releases() != asked {
		t.Errorf("the resume asked the relay for B again (%d releases, %d before)", releases(), asked)
	}
	res, _ := ReadResult(l.w.state, spec.RunID)
	held(res)
	// the parent's decision on the hold closes B by escalation; the resume asks the relay nothing for B
	j, err := journal.Open(context.Background(), l.w.st, spec.RunID, journal.Identity{}, journal.Options{Immediate: true})
	if err != nil {
		t.Fatal(err)
	}
	j.Append(engine.Event{RunID: spec.RunID, Kind: engine.KindFlow, Event: ops.EvDecisionApplied, Data: map[string]any{"ask": id, "node": b, "decision": map[string]any{"id": "dec-hold"}}})
	if err := j.Close(); err != nil {
		t.Fatal(err)
	}
	if code, _ := l.process(spec.RunID, true); code != 0 || releases() != asked {
		t.Fatalf("the resume after the decision exited %d with %d releases of B", code, releases())
	}
	res, _ = ReadResult(l.w.state, spec.RunID)
	var v laneValue
	if err := json.Unmarshal(res.Value, &v); err != nil || v.State != "incomplete" {
		t.Fatalf("after the decision: %s", res.Value)
	}
	for _, ln := range v.Lanes {
		if ln.Node == b && (ln.State != "escalated" || ln.Reason != ops.ReasonPlanSuperseded) {
			t.Errorf("lane B after the decision: %+v", ln)
		}
	}
}

// TestSchedRelayPlanChangedNamesTheRevisionsAndReturnsTheSlot: a release the relay refuses plan_changed names the revision the run pinned,
// the plan's head revision and the node's execution the relay still holds with its run (here the run's own, committed under the slice
// before the plan changed, the replay CAS of CRW-1288); the hold's close of it (plan_superseded) ends it and returns its slot.
func TestSchedRelayPlanChangedNamesTheRevisionsAndReturnsTheSlot(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"})
	r := l.relayFor("run-hold000001", l.handoffBytes(nil), 1)
	node := l.nodeOf["A-1"]
	slice := func() string {
		for _, n := range l.snapshot(0).Nodes {
			if n.NodeID == node {
				return n.SliceDigest
			}
		}
		return ""
	}
	first, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: slice()})
	if err != nil || !first.Released {
		t.Fatalf("the release: %+v %v", first, err)
	}
	l.retitle(r, "A-1")
	again, err := r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: node, RunID: r.RunID, RequestID: "req-1", SliceDigest: slice()})
	if err != nil || again.Released || again.Reason != ops.ReasonPlanChanged || again.PinnedRevision != 1 || again.HeadRevision != 2 ||
		again.HeldExecution != first.ExecutionID || again.HeldRun != r.RunID {
		t.Fatalf("the refused replay: %+v %v", again, err)
	}
	held := "SELECT COUNT(*) FROM execution_slots WHERE subject_kind = 'dag_node' AND subject_key = ? AND state = 'held'"
	if n := l.count(held, l.plan+"/"+node); n != 1 {
		t.Fatalf("%d held slots before the hold", n)
	}
	closed, err := r.Close(ctx, ops.CloseRequest{Plan: l.plan, Node: node, Actor: labOwner, Reason: ops.ReasonPlanSuperseded, RequestID: "req-hold"})
	if err != nil || !closed.Closed || !closed.SlotReleased || closed.ExecutionID != first.ExecutionID {
		t.Fatalf("the hold's close: %+v %v", closed, err)
	}
	if n := l.count(held, l.plan+"/"+node); n != 0 {
		t.Errorf("%d held slots after the hold", n)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_id = ? AND reason = ? AND state = 'cancelled' AND slot_released = 1", first.ExecutionID, ops.ReasonPlanSuperseded); n != 1 {
		t.Errorf("%d closures of the execution", n)
	}
}

// TestPlanSupersededHoldReturnsTheRunsSlotEndToEnd (d1 of the evaluation of 157e0c3d): B has a live execution of the run committed under
// the slice of the relay's new revision (the stray release a refused replay leaves, CRW-1288 P3). The run's release of B is refused and
// raises the hold, which closes that execution (its slot returns); killed after the close, the resumed raising call closes it again.
func TestPlanSupersededHoldReturnsTheRunsSlotEndToEnd(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "B-1", Milestone: "M1", After: []string{"A-1"}})
	a, b := l.nodeOf["A-1"], l.nodeOf["B-1"]
	var stray, closed = ops.ReleaseResult{}, false
	l.after = func(r *SchedRelay, effect string) {
		if effect == "close:"+b && !closed {
			closed = true
			l.kill()
		}
		if effect != "integrate:"+a || stray.ExecutionID != "" {
			return
		}
		l.retitle(r, "B-1")
		var err error // no expected slice: the relay commits the release under the slice of its head (revision 2)
		if stray, err = r.Release(ctx, ops.ReleaseRequest{Plan: l.plan, Node: b, RunID: r.RunID, RequestID: "req-stray"}); err != nil || !stray.Released {
			t.Errorf("the stray release of B: %+v %v", stray, err)
		}
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	res, effects := l.runToEnd(spec.RunID), strings.Join(l.effects, " ")+" "
	if v := (laneValue{}); json.Unmarshal(res.Value, &v) != nil || v.State != "held" || l.processes != 2 || strings.Count(effects, "close:"+b+" ") != 2 || strings.Count(effects, "release:"+b+" ") != 1 {
		t.Fatalf("the run: %s %s, %d processes, the relay asked: %s", res.Status, res.Value, l.processes, effects)
	}
	var hold []map[string]any
	for _, e := range l.events() {
		if evidence, _ := e.Data["evidence"].(map[string]any); e.Kind == engine.KindFlow && e.Event == engine.EvEscalation && e.Data["reason"] == ops.ReasonPlanSuperseded {
			hold = append(hold, evidence)
		}
	}
	if len(hold) != 1 || hold[0]["held_execution"] != stray.ExecutionID || hold[0]["held_run"] != spec.RunID || hold[0]["slot"] != "returned" {
		t.Fatalf("the holds record %v, want the stray execution %s of the run, its slot returned", hold, stray.ExecutionID)
	}
	if n := l.count("SELECT COUNT(*) FROM execution_slots WHERE subject_key = ? AND state = 'held'", l.plan+"/"+b); n != 0 || l.count("SELECT COUNT(*) FROM dag_agent_closures WHERE subject_id = ? AND reason = ? AND state = 'cancelled' AND slot_released = 1", stray.ExecutionID, ops.ReasonPlanSuperseded) != 1 {
		t.Errorf("%d held slots of B; the closures of the stray execution are not one cancelled plan_superseded close that returned the slot", n)
	}
}
