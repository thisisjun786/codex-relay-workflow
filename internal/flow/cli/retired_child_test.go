package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
)

// The tests the managed `child` session start had (cli/childhost_test.go, CRW-1285, CRW-1382), on the path that replaced it (CRW-1464):
// a node sized for a child starts no `child` session, and a node sized for a split, which a DESIGN of S-1 gives an issue too large for
// one build, is released to one executor under the slice the flow compared.

// TestAChildSizedNodeStartsNoManagedChild is the successor of TestReleaseChildStartsAManagedChildWithHostSettings (CRW-1285 (2),
// SP-CHILD-1), the test of the managed `child` session start CRW-1464 removed: a node a DESIGN of before S-1 sized for a child starts no `child` session on the run's relay. Its
// crw.release row is the unreleased sized row (kind child, reason size_fit_child, no execution, no slice), the relay holds no execution,
// release request or managed start for it and was asked no release of it, no call of the node is made, and the node escalates once as
// size_split with detail child_retired while the other node lands; a resume of the run asks the relay nothing for it either.
func TestAChildSizedNodeStartsNoManagedChild(t *testing.T) {
	lane := legacyChildLane(t)
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "A-2", Milestone: "M1"})
	l.legacyChildDesign("A-2")
	node := l.nodeOf["A-2"]
	spec := l.start("start", lane, "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	var v laneValue
	_ = json.Unmarshal(res.Value, &v)
	lanes := map[string]string{}
	for _, ln := range v.Lanes {
		lanes[ln.Node] = ln.State + "/" + ln.Reason
	}
	if lanes[node] != "escalated/size_split" || !strings.HasPrefix(lanes[l.nodeOf["A-1"]], "landed") {
		t.Fatalf("the lanes %v (run %s %s %s)\n%s", lanes, res.Status, res.ErrorKind, res.Error, res.Value)
	}
	rows := l.releaseRowsOf(node)
	if len(rows) != 1 || str(rows[0].Data, "kind") != "child" || dataBoolOf(rows[0].Data, "released") || str(rows[0].Data, "reason") != "size_fit_child" ||
		str(rows[0].Data, "execution_id") != "" || str(rows[0].Data, "slice_digest") != "" {
		t.Fatalf("the release rows of the child-sized node %+v, want one unreleased sized row of kind child", rows)
	}
	noChild := func(when string) {
		t.Helper()
		if n := l.count("SELECT COUNT(*) FROM dag_agent_executions WHERE node_id = ?", node) + l.count("SELECT COUNT(*) FROM dag_node_executions WHERE node_id = ?", node) +
			l.count("SELECT COUNT(*) FROM dag_release_requests WHERE node_id = ?", node) + l.count("SELECT COUNT(*) FROM managed_start_requests"); n != 0 {
			t.Errorf("%s: the relay holds %d executions, release requests or managed starts of the child-sized node", when, n)
		}
		for _, e := range l.effects {
			if e == "release:"+node {
				t.Errorf("%s: the relay was asked to release the child-sized node", when)
			}
		}
		for _, ev := range l.events() {
			if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared && str(ev.Data, "node") == node {
				t.Errorf("%s: a call %s of the child-sized node", when, ev.Label)
			}
		}
	}
	noChild("the run")
	var esc []engine.Event
	for _, ev := range l.events() {
		if ev.Kind == engine.KindFlow && ev.Event == engine.EvEscalation && str(ev.Data, "node") == node {
			esc = append(esc, ev)
		}
	}
	if len(esc) != 1 || str(esc[0].Data, "reason") != "size_split" || str(esc[0].Data, "detail") != "child_retired" {
		t.Errorf("the escalations of the child-sized node %+v, want one size_split with detail child_retired", esc)
	}
	if code, _ := l.process(spec.RunID, true); code != 0 {
		t.Fatalf("the resume exited %d", code)
	}
	noChild("the resume")
	if n := len(l.releaseRowsOf(node)); n != 1 {
		t.Errorf("%d release rows of the child-sized node after the resume, want the one", n)
	}
}

// TestSchedRelaySplitReleaseComparesTheExpectedSlice is the successor of TestSchedRelayReleaseChildComparesTheExpectedSlice (CRW-1382
// d3), the slice test of the `child` session start CRW-1464 removed: the release of a node sized for a split (the size a DESIGN of S-1 gives an issue too large for one build) is one executor's, and it carries the slice
// the flow compared, as every release does (CRW-1288). A-1 is released under the slice of the plan the run pinned (its release row names
// that slice) and runs its sub-tasks on one executor thread; after A-1 integrates, a plan revision retitles B-1, so the plan's slice of
// B-1 is another: B-1 is released to no executor (the relay holds no execution of it, no call of it is made) and the run holds
// plan_superseded.
func TestSchedRelaySplitReleaseComparesTheExpectedSlice(t *testing.T) {
	l := newLaneLab(t, labIssue{Key: "A-1", Milestone: "M1"}, labIssue{Key: "B-1", Milestone: "M1", After: []string{"A-1"}})
	a, b := l.nodeOf["A-1"], l.nodeOf["B-1"]
	slice := ""
	for _, n := range l.snapshot(0).Nodes {
		if n.NodeID == a {
			slice = n.SliceDigest
		}
	}
	l.designEdit = func(d *ops.Design) {
		for i := range d.Issues {
			own := d.Issues[i].EditRegions[0]
			d.Issues[i].SizeFit, d.Issues[i].SizeReason, d.Issues[i].IssueSizeDecision = "split", "regions_gt3", "over_line"
			d.Issues[i].SubTasks = []ops.SubTask{{ID: "one", Summary: "the first part", EditRegions: []string{own}, DoneCondition: "the first part is written"},
				{ID: "two", Summary: "the second part", EditRegions: []string{own}, DoneCondition: "the second part is written", After: []string{"one"}}}
		}
	}
	moved := false
	l.after = func(r *SchedRelay, effect string) {
		if effect == "integrate:"+a && !moved {
			moved = true
			l.retitle(r, "B-1")
		}
	}
	spec := l.start("start", "project-lane", "--args", `{"plan":"plan-1","milestone":"M1"}`, "--handoff", l.file)
	res := l.runToEnd(spec.RunID)
	var v laneValue
	if json.Unmarshal(res.Value, &v) != nil || v.State != "held" || !moved {
		t.Fatalf("the run: %s %s %s (A integrated: %v)", res.Status, res.Error, res.Value, moved)
	}
	for _, ln := range v.Lanes {
		switch {
		case ln.Node == a && ln.State != "landed":
			t.Errorf("lane A: %+v", ln)
		case ln.Node == b && (ln.State != "held" || ln.Reason != ops.ReasonPlanSuperseded):
			t.Errorf("lane B: %+v", ln)
		}
	}
	rows := l.releaseRowsOf(a)
	if len(rows) != 1 || !dataBoolOf(rows[0].Data, "released") || str(rows[0].Data, "kind") != "executor" || slice == "" || str(rows[0].Data, "slice_digest") != slice {
		t.Errorf("the release rows of the split node A %+v, want one executor release under the slice %s", rows, slice)
	}
	var labels []string
	for _, ev := range l.events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvPrepared {
			switch str(ev.Data, "node") {
			case a:
				labels = append(labels, ev.Label)
			case b:
				t.Errorf("a call %s of B, whose slice the plan changed", ev.Label)
			}
		}
	}
	if !strings.Contains(strings.Join(labels, " "), "subtask:"+a+":executor:s2") {
		t.Errorf("the calls of A %v, want its second sub-task on its thread", labels)
	}
	if n := l.count("SELECT COUNT(*) FROM dag_agent_executions WHERE node_id = ?", b); n != 0 {
		t.Errorf("B has %d executions", n)
	}
}

// dataBoolOf is the boolean member key of a row's data (false when it is absent or not a boolean).
func dataBoolOf(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}
