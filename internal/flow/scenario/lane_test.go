package scenario

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/bridge/appserver/fakehost"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
)

type fakehostPlan = fakehost.TurnPlan

// twoNodes are two issue nodes with their own files: A, and B which waits for A.
func twoNodes() []NodeSpec {
	return []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}, After: []string{"A"}},
	}
}

type laneResult struct {
	State string `json:"state"`
	Tip   string `json:"tip"`
	Lanes []struct {
		Node   string `json:"node"`
		State  string `json:"state"`
		Reason string `json:"reason"`
		Head   string `json:"head"`
	} `json:"lanes"`
}

func mustRun(t *testing.T, l *Lab) laneResult {
	t.Helper()
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s\ncalls: %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	got := Value[laneResult](t, res)
	if got.State != "closed" {
		t.Logf("value: %s", res.Value)
		for _, ev := range l.FlowRows(engine.EvEscalation) {
			t.Logf("escalation: %v", ev.Data)
		}
	}
	return got
}

func index(list []string, want string) int { return slices.Index(list, want) }

// The happy path: the architect plans the milestone, an independent reviewer audits the DESIGN, each node is released, built by
// an executor, committed, verified, reviewed by a reviewer of another family and landed, the milestone closes over the integrated
// range and the report goes up. No tier step is taken.
func TestLaneHappyPath(t *testing.T) {
	l := NewLab(t, twoNodes()...)
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "build:B:executor", "review:B:r1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	if roles := l.Roles(); !slices.Equal(roles, []string{"architect", "reviewer", "executor", "reviewer", "executor", "reviewer"}) {
		t.Fatalf("roles %v", roles)
	}
	if !slices.Equal(l.Relay.Landed, []string{"A", "B"}) {
		t.Fatalf("landed %v", l.Relay.Landed)
	}
	tip := l.Tip()
	for _, f := range []string{"node-A.txt", "node-B.txt"} {
		if out, err := l.gitErr(l.Repo, "cat-file", "-e", tip+":"+f); err != nil {
			t.Errorf("%s is not in the integration branch: %v %s", f, err, out)
		}
	}
	if got.Tip != tip {
		t.Errorf("the close judged %s, the branch is at %s", got.Tip, tip)
	}
	for _, op := range []string{"crw.milestone", "crw.designAudit", "crw.designLedger", "crw.release", "crw.issuePlan", "crw.commit", "crw.verify", "crw.review", "crw.accept", "crw.integrate", "crw.milestoneClose", "crw.reportUp"} {
		if len(l.Rows(op)) == 0 {
			t.Errorf("no completed %s row", op)
		}
	}
	if steps := l.StepRows(); len(steps) != 0 {
		t.Errorf("a tier step in the happy path: %+v", steps)
	}
	if strings.Contains(strings.Join(l.Roles(), " "), "executor-t1") {
		t.Errorf("executor-t1 in the happy path")
	}
	_ = index
}

// ---- helpers shared by the scenarios ----

// oneNode is a plan of one node.
func oneNode() []NodeSpec {
	return []NodeSpec{{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}}}
}

// reviews scripts the verdicts of the reviewer calls of a node, one for each round.
func reviews(l *Lab, node string, answers ...map[string]any) {
	plans := make([]func(*Call) fakehostPlan, len(answers))
	for i, a := range answers {
		a := a
		plans[i] = func(*Call) fakehostPlan { return Say(a) }
	}
	l.Sequence("^review:"+node+":r\\d+$", plans...)
}

// noTierStep asserts the run took no tier step: no executor-t1 call among the calls, no step row, no crw.tierStep row.
func noTierStep(t *testing.T, l *Lab) {
	t.Helper()
	for _, c := range l.Calls() {
		if c.Role == "executor-t1" {
			t.Errorf("an executor-t1 call %q in the calls %v", c.Label, l.Labels())
		}
	}
	if steps := l.StepRows(); len(steps) != 0 {
		t.Errorf("step decisions %+v", steps)
	}
	if rows := l.Rows("crw.tierStep"); len(rows) != 0 {
		t.Errorf("%d crw.tierStep rows", len(rows))
	}
	for _, r := range l.Requests("thread/start") {
		if strings.Contains(string(r.Params), "claude-fake-sonnet") {
			t.Errorf("a thread was started on the executor-t1 pair: %s", r.Params)
		}
	}
}

// laneOf is the result of one node's lane.
func laneOf(t *testing.T, got laneResult, node string) (state, reason string) {
	t.Helper()
	for _, ln := range got.Lanes {
		if ln.Node == node {
			return ln.State, ln.Reason
		}
	}
	t.Fatalf("no lane result for node %s in %+v", node, got.Lanes)
	return "", ""
}

// callNamed is the call with the label.
func callNamed(t *testing.T, l *Lab, label string) CallInfo {
	t.Helper()
	for _, c := range l.Calls() {
		if c.Label == label {
			return c
		}
	}
	t.Fatalf("no call labelled %q among %v", label, l.Labels())
	return CallInfo{}
}

// rowsAfter are the rows of the journal after seq.
func rowsAfter(l *Lab, seq int) []engine.Event {
	var out []engine.Event
	for _, ev := range l.Events() {
		if ev.Seq > seq {
			out = append(out, ev)
		}
	}
	return out
}

// textOfCall is the text of the first turn the call was sent.
func textOfCall(l *Lab, callID string) string {
	for _, r := range l.Requests("turn/start") {
		var p map[string]any
		_ = json.Unmarshal(r.Params, &p)
		text := inputText(p)
		if strings.HasPrefix(text, "crw-flow-op: "+callID+"/") {
			return text
		}
	}
	return ""
}

// ---- the review loop ----

// A reviewer that says needs_changes twice with different findings, then verified: the executor fixes on its own thread and
// worktree each time, the reviewer is a fresh thread each round, and nothing steps the tier.
func TestTwoNeedsChangesThenVerified(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviews(l, "A",
		Verdict(false, Defect("d1", "correctness", "P2", "the first thing is wrong")),
		Verdict(false, Defect("d2", "test_gap", "P2", "a different thing is missing")),
		Verdict(true))
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "fix:A:executor:r1", "review:A:r2", "fix:A:executor:r2", "review:A:r3"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	build, fix1, fix2 := callNamed(t, l, "build:A:executor"), callNamed(t, l, "fix:A:executor:r1"), callNamed(t, l, "fix:A:executor:r2")
	if fix1.Resume != build.ID || fix2.Resume != fix1.ID {
		t.Errorf("the fixes continue %q and %q, want the executor's own thread: %s then %s", fix1.Resume, fix2.Resume, build.ID, fix1.ID)
	}
	if text := textOfCall(l, fix1.ID); !strings.Contains(text, "the first thing is wrong") {
		t.Errorf("the first fix packet does not carry the finding:\n%s", text)
	}
	threads := map[string]bool{}
	for _, c := range l.CallsOf("reviewer") {
		if strings.HasPrefix(c.Label, "review:") {
			threads[c.ID] = true
		}
	}
	if len(threads) != 3 {
		t.Errorf("%d review calls, want 3 fresh ones", len(threads))
	}
	if n := len(l.Rows("crw.commit")); n != 3 {
		t.Errorf("%d commits, want one for each round", n)
	}
	noTierStep(t, l)
}
