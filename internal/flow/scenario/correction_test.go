package scenario

import (
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// The tests of the correction round after the pre-merge evaluation of bb50b215 (CRW-1201 d1 to d7). Each is red on that head.

// verdictOver is a verdict over the criteria with the ids, every one met (or none, for needs_changes).
func verdictOver(verified bool, ids ...string) map[string]any {
	v := Verdict(verified)
	var crit []any
	for _, id := range ids {
		crit = append(crit, map[string]any{"id": id, "met": verified, "evidence": "read the head for " + id})
	}
	v["criteria"] = crit
	return v
}

// d1: another landing moves the integration target while a node is being reviewed. The verification and the acceptance of the node
// stand on one base, and it is an ancestor of the node's head: the commit its worktree grew from, not whatever the target is at when
// each step looks.
func TestLandingWhileReviewKeepsOneAncestorBase(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	start := l.Tip()
	l.On("^review:B:r1$", func(*Call) *fakehostPlan { l.AdvanceIntegration("outside.txt"); return nil })
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v; escalations %v", got.State, got, l.FlowRows(engine.EvEscalation))
	}
	if len(l.Relay.Accepts) != 2 {
		t.Fatalf("%d acceptances", len(l.Relay.Accepts))
	}
	for _, a := range l.Relay.Accepts {
		if out, err := l.gitErr(l.Repo, "merge-base", "--is-ancestor", a.Base, a.Head); err != nil {
			t.Errorf("node %s: the acceptance base %s is not an ancestor of the head %s: %s", a.Node, a.Base, a.Head, out)
		}
		if a.Base != start {
			t.Errorf("node %s: the acceptance base is %s, the commit the worktree grew from is %s", a.Node, a.Base, start)
		}
		for _, v := range l.Verify.Calls {
			if v.Node == a.Node && v.Head == a.Head && v.Base != a.Base {
				t.Errorf("node %s: verified over base %s, accepted over base %s", a.Node, v.Base, a.Base)
			}
		}
		rec, err := premerge.Decode(a.Premerge)
		if err != nil || rec.Dev != a.Base {
			t.Errorf("node %s: the premerge record stands on %q (%v), the acceptance on %s", a.Node, rec.Dev, err, a.Base)
		}
	}
	if !slices.Equal(slices.Sorted(slices.Values(l.Relay.Landed)), []string{"A", "B"}) {
		t.Errorf("landed %v", l.Relay.Landed)
	}
}

// d2: a verification that never ran is held, and it is asked again when the verifier is back: the resume does not replay the
// "did not run" answer.
func TestVerifierBackAfterResumeVerifiesAgain(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Unavailable = map[string]bool{"A": true}
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "verify_unavailable" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	before := l.LastSeq()
	l.Verify.Unavailable = nil
	l.NewProcess("proc-2")
	res := l.Resume()
	final := Value[laneResult](t, res)
	if final.State != "closed" {
		t.Fatalf("resume: %+v; labels %v", final, l.Labels())
	}
	if n := len(l.Verify.Calls); n != 2 {
		t.Errorf("%d verifications, want the failed start and the one after the resume", n)
	}
	for _, c := range l.LiveCalls(before) {
		if strings.HasPrefix(c.Label, "build:") {
			t.Errorf("the resume ran the executor again: %q", c.Label)
		}
	}
	if !slices.Equal(l.Relay.Landed, []string{"A"}) {
		t.Errorf("landed %v", l.Relay.Landed)
	}
}

// d3: the explorer of an evidence question reads the committed head the reviewer asked about, not the integration base.
func TestEvidenceExplorerReadsTheCommittedHead(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviews(l, "A", Verdict(false, EvidenceDefect("e1", "does the retry loop stop at the deadline?", "node-A.txt")), Verdict(true))
	var seen struct{ cwd, file string }
	l.On("^explore:A:", func(c *Call) *fakehostPlan {
		seen.cwd = c.Cwd
		_, err := l.gitErr(c.Cwd, "cat-file", "-e", "HEAD:node-A.txt")
		if err == nil {
			seen.file = "present"
		}
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	commits := l.Rows("crw.commit")
	if len(commits) == 0 {
		t.Fatal("no commit row")
	}
	head := str(commits[0].Data, "commit")
	exp := callNamed(t, l, l.Labels()[slices.IndexFunc(l.Labels(), func(s string) bool { return strings.HasPrefix(s, "explore:A:") })])
	prep := l.Prepared(exp.ID)
	if str(prep.Data, "head") != head {
		t.Errorf("the explorer call names head %q, the reviewed head is %s", str(prep.Data, "head"), head)
	}
	if seen.file != "present" {
		t.Errorf("the explorer's worktree %q does not hold the head's file node-A.txt: it was made at the integration base", seen.cwd)
	}
}

// d4: a node released as an issue child starts its child once: a resume that recovers another node does not release it again.
func TestChildIsStartedOnceAcrossAResume(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "a big issue", Regions: []string{"a/"}, SizeFit: "child", SizeReason: "regions_gt3"},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	l.Verify.Unavailable = map[string]bool{"B": true}
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "B"); state != "held" || reason != "verify_unavailable" {
		t.Fatalf("lane B: %s %s", state, reason)
	}
	if len(l.Relay.Children) != 1 {
		t.Fatalf("children after the first run: %+v", l.Relay.Children)
	}
	l.Verify.Unavailable = nil
	l.NewProcess("proc-2")
	res := l.Resume()
	if state, _ := laneOf(t, Value[laneResult](t, res), "B"); state != "landed" {
		t.Fatalf("lane B after the resume: %s", state)
	}
	if len(l.Relay.Children) != 1 {
		t.Errorf("the child of A was started %d times: %+v", len(l.Relay.Children), l.Relay.Children)
	}
	if rows := l.Rows("crw.releaseChild"); len(rows) != 1 {
		t.Errorf("%d releaseChild rows", len(rows))
	}
}

// d5: a verified verdict names every criterion of the node's plan once. One that leaves a criterion out is no verdict (the reviewer
// is asked once more on a fresh thread, then the node escalates); one that names an id twice, or an id the plan does not have, is
// the same.
func TestVerdictMustCoverEveryCriterionOnce(t *testing.T) {
	for name, answer := range map[string]map[string]any{
		"one of two": verdictOver(true, "c1"),
		"twice":      verdictOver(true, "c1", "c1"),
		"unknown":    verdictOver(true, "c1", "c3"),
	} {
		t.Run(name, func(t *testing.T) {
			nodes := []NodeSpec{{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}, Criteria: []string{"first criterion", "second criterion"}}}
			l := NewLab(t, nodes...)
			reviews(l, "A", answer)
			got := mustRunIncomplete(t, l)
			if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "reviewer_null_model" {
				t.Fatalf("lane A: %s %s", state, reason)
			}
			if len(l.Relay.Accepts) != 0 || len(l.Relay.Landed) != 0 {
				t.Errorf("an incomplete verdict reached the acceptance: %+v", l.Relay.Accepts)
			}
			if n := len(l.CallsOf("reviewer")) - 1; n != 2 {
				t.Errorf("%d review calls, want the first and the one more on a fresh thread", n)
			}
		})
	}
	nodes := []NodeSpec{{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}, Criteria: []string{"first criterion", "second criterion"}}}
	l := NewLab(t, nodes...)
	reviews(l, "A", verdictOver(true, "c1", "c2"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("a verdict over both criteria: %+v", got)
	}
	rec, err := premerge.Decode(l.Relay.Accepts[0].Premerge)
	if err != nil || len(rec.Criteria) != 2 {
		t.Errorf("the premerge record holds %d criteria (%v)", len(rec.Criteria), err)
	}
}

// d6: the nodes the lane runs at once do not overlap in the regions the DESIGN gives them. The plan declared a/ and b/; the approved
// DESIGN gives both nodes shared/, so the second executor starts after the first has landed.
func TestAdmissionFollowsTheEditRegionsOfTheDesign(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	d := l.designFor()
	for i := range d.Issues {
		d.Issues[i].EditRegions = []string{"shared/"}
	}
	l.Design = &d
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	labels := l.Labels()
	a, b := slices.Index(labels, "build:A:executor"), slices.Index(labels, "build:B:executor")
	if a < 0 || b < 0 || b < slices.Index(labels, "review:A:r1") {
		t.Errorf("calls %v: B's executor started before A's lane ended although both edit shared/", labels)
	}
}
