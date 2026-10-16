package scenario

import (
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/acceptance/premerge"
)

// The tests of the correction round after the pre-merge evaluation of bb50b215 (CRW-1201 d1 to d7). Each is red on that head.

// d1: another landing moves the integration target while a node is between its verification and its acceptance (where its review
// was before the unit check, CRW-1465). The verification and the acceptance of the node stand on one base, and it is an ancestor of
// the node's head: the commit its worktree grew from, not whatever the target is at when each step looks.
func TestLandingWhileReviewKeepsOneAncestorBase(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	start := l.Tip()
	var advanced sync.Once
	l.OnRow(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpVerify && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "B" {
			advanced.Do(func() { l.AdvanceIntegration("outside.txt") })
		}
	})
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

// landingBeforeVerifyLab is the world of the stableBase tests: A and B grow from one commit, and A lands while B is building, so B
// is verified and accepted after the integration tip has moved past the commit B grew from.
func landingBeforeVerifyLab(t *testing.T) (*Lab, string) {
	t.Helper()
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	start := l.Tip()
	bStarted := make(chan struct{})
	aLanded := make(chan struct{})
	var bOnce, aOnce sync.Once
	l.OnRow(func(ev engine.Event) {
		if ev.Op == ops.OpIntegrate && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "A" {
			aOnce.Do(func() { close(aLanded) })
		}
	})
	// A's unit check answers once B's executor has started
	l.UnitChecks("A", UnitCheck{Gate: bStarted})
	l.On("^build:B:executor$", func(c *Call) *fakehostPlan {
		c.Edit()
		bOnce.Do(func() { close(bStarted) })
		p := Say(Receipt("ready_for_review"))
		p.Gate = aLanded
		return &p
	})
	return l, start
}

// d1 (stableBase): another node lands while B is building, before B is verified. Both worktrees grew from the same commit, so when
// B is verified and accepted after A has moved the integration tip, stableBase finds the merge-base of B's head and the moved tip:
// the one common ancestor of the two heads.
func TestLandingBeforeVerifyKeepsCommonAncestorBase(t *testing.T) {
	l, start := landingBeforeVerifyLab(t)
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v; escalations %v", got.State, got, l.FlowRows(engine.EvEscalation))
	}
	if !slices.Equal(l.Relay.Landed, []string{"A", "B"}) {
		t.Fatalf("landed %v, want [A B]", l.Relay.Landed)
	}
	var aIntegrateSeq, bVerifySeq int
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted {
			if ev.Op == ops.OpIntegrate && str(ev.Data, "node") == "A" {
				aIntegrateSeq = ev.Seq
			}
			if ev.Op == ops.OpVerify && str(ev.Data, "node") == "B" {
				bVerifySeq = ev.Seq
			}
		}
	}
	if aIntegrateSeq == 0 || bVerifySeq <= aIntegrateSeq {
		t.Fatalf("A integrated at seq %d, B verified at seq %d: A must land before B is verified", aIntegrateSeq, bVerifySeq)
	}
	if len(l.Relay.Accepts) != 2 {
		t.Fatalf("%d acceptances", len(l.Relay.Accepts))
	}
	heads := map[string]string{}
	for _, a := range l.Relay.Accepts {
		heads[a.Node] = a.Head
	}
	common := l.Git("merge-base", heads["A"], heads["B"])
	if common != start {
		t.Fatalf("merge-base of A (%s) and B (%s) is %s, want %s", heads["A"], heads["B"], common, start)
	}
	for _, a := range l.Relay.Accepts {
		if out, err := l.gitErr(l.Repo, "merge-base", "--is-ancestor", a.Base, a.Head); err != nil {
			t.Errorf("node %s: the acceptance base %s is not an ancestor of the head %s: %s", a.Node, a.Base, a.Head, out)
		}
		if a.Base != common {
			t.Errorf("node %s: the acceptance base is %s, the common ancestor of the two heads is %s", a.Node, a.Base, common)
		}
		for _, v := range l.Verify.Calls {
			if v.Node == a.Node && v.Head == a.Head && v.Base != common {
				t.Errorf("node %s: verified over base %s, want common ancestor %s", a.Node, v.Base, common)
			}
		}
		rec, err := premerge.Decode(a.Premerge)
		if err != nil || rec.Dev != common {
			t.Errorf("node %s: the premerge record stands on %q (%v), want %s", a.Node, rec.Dev, err, common)
		}
	}
}

// CRW-1365 d1 (the fallback of crw.accept): a verification row that names no base (a row written before the verification recorded
// its base) is judged over stableBase found at the acceptance, the merge-base of B's head and the moved tip: the commit B grew
// from, not the integration tip A moved.
func TestVerificationWithoutBaseIsAcceptedOverCommonAncestor(t *testing.T) {
	l, start := landingBeforeVerifyLab(t)
	l.Rewrite(func(ev engine.Event) engine.Event {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpVerify && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "B" {
			data := maps.Clone(ev.Data)
			delete(data, "base")
			ev.Data = data
		}
		return ev
	})
	got := mustRun(t, l)
	var verB engine.Event
	for _, ev := range l.Rows(ops.OpVerify) {
		if str(ev.Data, "node") == "B" {
			verB = ev
		}
	}
	if verB.Seq == 0 || str(verB.Data, "base") != "" {
		t.Fatalf("B's verification row %v: the scenario needs one that names no base", verB.Data)
	}
	var accB *ops.AcceptRequest
	for i, a := range l.Relay.Accepts {
		if a.Node == "B" {
			accB = &l.Relay.Accepts[i]
		}
	}
	if accB == nil {
		t.Fatalf("no acceptance of B: %+v", l.Relay.Accepts)
	}
	if tip := l.Tip(); accB.Base != start {
		t.Errorf("B is accepted over %s, want the commit it grew from %s (the integration tip is %s)", accB.Base, start, tip)
	}
	if common := l.Git("merge-base", start, accB.Head); common != start {
		t.Errorf("the commit B grew from %s is not the merge-base %s of B's head", start, common)
	}
	if rec, err := premerge.Decode(accB.Premerge); err != nil || rec.Dev != start {
		t.Errorf("B's premerge record stands on %q (%v), want %s", rec.Dev, err, start)
	}
	if got.State != "closed" || !slices.Equal(l.Relay.Landed, []string{"A", "B"}) {
		t.Errorf("state %q, landed %v; escalations %v", got.State, l.Relay.Landed, l.FlowRows(engine.EvEscalation))
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
	nodeCalls := 0
	for _, c := range l.Verify.Calls {
		if c.Node == "A" {
			nodeCalls++
		}
	}
	if nodeCalls != 2 {
		t.Errorf("%d verifications of the node, want the failed start and the one after the resume", nodeCalls)
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

// d3: the unit check reads the work it judges (CRW-1465): the checker is given the node's worktree with the executor's work
// uncommitted in it, and the tree it saw is the tree of the commit the run makes after it, the head the acceptance stands on.
func TestUnitCheckReadsTheUncommittedWorkTheCommitRecords(t *testing.T) {
	l := NewLab(t, oneNode()...)
	var seen struct{ dir, status string }
	l.UnitChecks("A", UnitCheck{Do: func() {
		calls := l.Checker.Calls("A")
		seen.dir = calls[len(calls)-1].Request.Worktree
		seen.status = l.GitIn(seen.dir, "status", "--porcelain", "--", "node-A.txt")
	}})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q", got.State)
	}
	commits, checks := l.Rows("crw.commit"), l.Rows(ops.OpUnitCheck)
	if len(commits) != 1 || len(checks) != 1 {
		t.Fatalf("%d commits and %d unit checks, want one of each", len(commits), len(checks))
	}
	build := callNamed(t, l, "build:A:executor")
	alloc, _ := l.Prepared(build.ID).Data["allocation"].(map[string]any)
	if seen.dir == "" || seen.dir != str(alloc, "path") {
		t.Errorf("the checker read %q, the node's worktree is %v", seen.dir, alloc["path"])
	}
	if !strings.Contains(seen.status, "node-A.txt") {
		t.Errorf("the checker's worktree does not hold the executor's uncommitted work (status %q)", seen.status)
	}
	if tree := str(commits[0].Data, "tree"); tree == "" || str(checks[0].Data, "tree") != tree || str(checks[0].Data, "call") != build.ID {
		t.Errorf("the check saw tree %q of call %q; the commit records tree %q of %s", str(checks[0].Data, "tree"), str(checks[0].Data, "call"), tree, build.ID)
	}
}

// d4: a node sized for a child escalates once (CRW-1415): a resume that recovers another node does not escalate it again, and no child
// is started in either run.
func TestChildSizedNodeEscalatesOnceAcrossAResume(t *testing.T) {
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
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "size_split" {
		t.Fatalf("lane A: %s %s, want escalated size_split", state, reason)
	}
	escalationsOfA := func() []engine.Event {
		var out []engine.Event
		for _, ev := range l.FlowRows(engine.EvEscalation) {
			if str(ev.Data, "node") == "A" {
				out = append(out, ev)
			}
		}
		return out
	}
	if esc := escalationsOfA(); len(esc) != 1 || str(esc[0].Data, "detail") != "child_retired" {
		t.Fatalf("escalations of A after the first run: %+v", esc)
	}
	l.Verify.Unavailable = nil
	l.NewProcess("proc-2")
	res := l.Resume()
	if state, _ := laneOf(t, Value[laneResult](t, res), "B"); state != "landed" {
		t.Fatalf("lane B after the resume: %s", state)
	}
	if esc := escalationsOfA(); len(esc) != 1 || str(esc[0].Data, "detail") != "child_retired" {
		t.Errorf("A escalated %d times across the resume: %+v", len(esc), esc)
	}
	if rows := l.Rows(ops.OpReleaseChild); len(rows) != 0 {
		t.Errorf("%d releaseChild rows", len(rows))
	}
}

// d5: the acceptance names every criterion of the node's plan once. On a unit check (CRW-1465) the check judges the change against the
// plan's criteria and crw.accept assembles a premerge record over each of them; a check that gives no verdict - an invalid answer,
// asked once more with the same request and invalid again - is no basis for an acceptance: the node escalates as a configuration
// question and nothing reaches the relay.
func TestVerdictMustCoverEveryCriterionOnce(t *testing.T) {
	nodes := []NodeSpec{{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}, Criteria: []string{"first criterion", "second criterion"}}}
	t.Run("invalid twice", func(t *testing.T) {
		l := NewLab(t, nodes...)
		l.UnitChecks("A", UnitCheck{State: reviewadapter.StateInvalid}, UnitCheck{State: reviewadapter.StateInvalid})
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "unit_check_invalid" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		if len(l.Relay.Accepts) != 0 || len(l.Relay.Landed) != 0 {
			t.Errorf("a check with no verdict reached the acceptance: %+v", l.Relay.Accepts)
		}
		if n := len(l.Checker.Calls("A")); n != 2 {
			t.Errorf("%d unit checks, want the first and the one more with the same request", n)
		}
		esc := l.FlowRows(engine.EvEscalation)
		if len(esc) != 1 || str(esc[0].Data, "ask") != "configuration" {
			t.Errorf("escalations %+v", esc)
		}
	})
	l := NewLab(t, nodes...)
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("a check over both criteria: %+v", got)
	}
	if calls := l.Checker.Calls("A"); len(calls) != 1 || !strings.Contains(calls[0].Request.Prompt, "c1: first criterion") || !strings.Contains(calls[0].Request.Prompt, "c2: second criterion") {
		t.Errorf("the unit check was not given both criteria: %+v", calls)
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
	if a < 0 || b < 0 || b < a {
		t.Errorf("calls %v: B's executor started before A's lane ended although both edit shared/", labels)
	}
	var aLanded engine.Event
	for _, r := range l.Rows("crw.integrate") {
		if str(r.Data, "node") == "A" {
			aLanded = r
		}
	}
	buildB := callNamed(t, l, "build:B:executor")
	if aLanded.Seq == 0 || buildB.Seq <= aLanded.Seq {
		t.Errorf("B's executor was prepared at seq %d before A landed at seq %d (calls %v)", buildB.Seq, aLanded.Seq, labels)
	}
	alloc, _ := l.Prepared(buildB.ID).Data["allocation"].(map[string]any)
	if str(alloc, "start_commit") != str(aLanded.Data, "batch_commit") {
		t.Errorf("B's worktree started at %v, A landed %v", alloc["start_commit"], aLanded.Data["batch_commit"])
	}
}

// d7: a suffix that runs crw.tierStep again at the same position over a new checkpoint gets another id from crw.stepRecord, and
// tierStep (crw:lib/lane) hands that recorded.id to executor-t1: the call and the persisted row carry the id the record gave.
func TestTierStepConflictHandsRecordedIDToExecutorT1(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"A": {1, 2, 3}}
	var killed sync.Once
	l.OnRow(func(ev engine.Event) {
		if ev.Op == "crw.stepRecord" && ev.Event == engine.EvCompleted {
			killed.Do(func() { l.Kill() })
		}
	})
	if res := l.Run(); res.Status == engine.RunCompleted {
		t.Fatalf("the first run completed (%s): it must end at the kill after its first step record", res.Value)
	}
	firstSteps := l.StepRows()
	if len(firstSteps) != 1 {
		t.Fatalf("step rows after the first run: %+v, want one", firstSteps)
	}
	commits := l.Rows("crw.commit")
	if len(commits) != 2 {
		t.Fatalf("%d commits before the kill, want two", len(commits))
	}
	c1, h1 := str(commits[0].Data, "commit"), str(commits[1].Data, "commit")
	if firstSteps[0].Head != h1 {
		t.Fatalf("first step checkpoint head %s, want %s", firstSteps[0].Head, h1)
	}
	build := callNamed(t, l, "build:A:executor")
	alloc, _ := l.Prepared(build.ID).Data["allocation"].(map[string]any)
	// no lab fixture moves a worktree back: the reset unproves the fix commit h1, so the resume reaches tierStep over a new checkpoint
	l.GitIn(str(alloc, "path"), "reset", "--hard", c1)

	l.NewProcess("proc-2")
	got := Value[laneResult](t, l.Resume())
	if got.State != "closed" {
		t.Fatalf("resume state %q: %+v", got.State, got)
	}
	steps := l.StepRows()
	if len(steps) != 2 {
		t.Fatalf("step rows %+v, want two (the first checkpoint and the conflict-resolved one)", steps)
	}
	commits = l.Rows("crw.commit")
	h2 := str(commits[2].Data, "commit")
	if h2 == h1 || steps[1].Head != h2 {
		t.Fatalf("second step checkpoint head %s, want the suffix commit %s (first was %s)", steps[1].Head, h2, h1)
	}
	// the id the record gave each step, read from the crw.stepRecord rows: the second, over the new checkpoint, is not the first
	records := l.Rows("crw.stepRecord")
	if len(records) != 2 {
		t.Fatalf("%d crw.stepRecord rows, want the one before the kill and the one of the resume", len(records))
	}
	recorded := str(records[1].Data, "step_id")
	if str(records[0].Data, "step_id") != steps[0].ID || recorded != steps[1].ID || recorded == steps[0].ID {
		t.Errorf("recorded step ids %q and %q, persisted %q and %q: the conflict must record a distinct id", str(records[0].Data, "step_id"), recorded, steps[0].ID, steps[1].ID)
	}
	t1 := l.CallsOf("executor-t1")
	if len(t1) != 1 {
		t.Fatalf("executor-t1 calls %+v, want one", t1)
	}
	if t1[0].Step != recorded {
		t.Errorf("the executor-t1 call carries step id %q, the record of checkpoint %s gave %q", t1[0].Step, h2, recorded)
	}
	t1Alloc, _ := l.Prepared(t1[0].ID).Data["allocation"].(map[string]any)
	if str(t1Alloc, "start_commit") != steps[1].Head {
		t.Errorf("executor-t1 started at %v, the persisted step row checkpoint is %s", t1Alloc["start_commit"], steps[1].Head)
	}
}
