package scenario

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/lint"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// A node the architect sized for a child is released as an issue child (a managed start of an issue-lane run), not to an
// executor: no executor call is made for it, no agent slot is reserved, the child is recorded with the design revision, and the
// other nodes of the milestone are not held up by it.
func TestSizeFitChildReleasesIssueChild(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "a big issue", Regions: []string{"a/"}, SizeFit: "child", SizeReason: "regions_gt3"},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	got := mustRunIncomplete(t, l)
	if got.State != "incomplete" {
		t.Fatalf("state %q: the child is not landed by this run", got.State)
	}
	if state, _ := laneOf(t, got, "A"); state != "child" {
		t.Fatalf("lane A: %s", state)
	}
	if state, _ := laneOf(t, got, "B"); state != "landed" {
		t.Fatalf("lane B: %s", state)
	}
	if len(l.Relay.Children) != 1 || l.Relay.Children[0].Node != "A" || l.Relay.Children[0].Reason != "regions_gt3" || l.Relay.Children[0].Revision != "r0" {
		t.Fatalf("children %+v", l.Relay.Children)
	}
	for _, c := range l.Calls() {
		if c.Node == "A" {
			t.Errorf("a call for the child node: %+v", c)
		}
	}
	for _, r := range l.Relay.Releases {
		if r.Node == "A" {
			t.Errorf("the child node was released to an executor: %+v", r)
		}
	}
	if l.Relay.SlotHeld("A") {
		t.Errorf("a slot is held for the child node")
	}
	seen := map[string]bool{}
	for _, r := range l.SpawnRows() {
		if r.Decision == "spawn" && r.Node == "A" {
			seen[r.Rule] = true
		}
	}
	if !seen[ops.RuleChild2] || !seen[ops.RuleChild1] || seen[ops.RuleExec1] {
		t.Errorf("decisions of A: %v: want the signal (SP-CHILD-2) and the child's spawn (SP-CHILD-1), and no executor", seen)
	}
	if rows := l.Rows("crw.releaseChild"); len(rows) != 1 || str(rows[0].Data, "child") != "child-A" {
		t.Errorf("releaseChild rows %+v", rows)
	}
}

// A group is an order unit only: its members run in the declared order, each with its own fresh executor on the executor tier (a
// tier step of the first does not carry to the second), the second one's worktree starts at the integration tip after the first
// landed, and no handle crosses from one node to the other.
func TestGroupRunsFreshExecutorPerMemberInOrder(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "first of the group", Regions: []string{"g/"}, Group: "G1"},
		{ID: "B", Issue: "CRW-B", Title: "second of the group", Regions: []string{"g/"}, Group: "G1", After: []string{"A"}},
	}
	l := NewLab(t, nodes...)
	same := Defect("d1", "correctness", "P1", "the guard is missing")
	reviews(l, "A", Verdict(false, same), Verdict(false, same), Verdict(true))
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	// A goes through a tier step; B starts on the executor tier
	var order []string
	for _, c := range l.Calls() {
		if c.Role == "executor" || c.Role == "executor-t1" {
			order = append(order, c.Role+":"+c.Node)
		}
	}
	if !slices.Equal(order, []string{"executor:A", "executor:A", "executor-t1:A", "executor:B"}) {
		t.Fatalf("executor calls %v", order)
	}
	if !slices.Equal(l.Relay.Landed, []string{"A", "B"}) {
		t.Fatalf("landed %v", l.Relay.Landed)
	}
	buildB := callNamed(t, l, "build:B:executor")
	if buildB.Resume != "" {
		t.Errorf("B's executor continues %q", buildB.Resume)
	}
	prep := l.Prepared(buildB.ID)
	if str(prep.Data, "model") != "claude-fake-haiku" || str(prep.Data, "effort") != "xhigh" || str(prep.Data, "step_decision") != "" {
		t.Errorf("B's executor runs on %v/%v with step %q: want haiku xhigh with none", prep.Data["model"], prep.Data["effort"], prep.Data["step_decision"])
	}
	alloc, _ := prep.Data["allocation"].(map[string]any)
	landedA := ""
	for _, r := range l.Rows("crw.integrate") {
		if str(r.Data, "node") == "A" {
			landedA = str(r.Data, "batch_commit")
		}
	}
	if landedA == "" || str(alloc, "start_commit") != landedA {
		t.Errorf("B's worktree starts at %v, A landed %s", alloc["start_commit"], landedA)
	}
	if out, err := l.gitErr(str2(alloc, "path"), "cat-file", "-e", "HEAD:node-A.txt"); err != nil {
		t.Errorf("B's worktree does not hold A's landed file: %v %s", err, out)
	}
	// A's handle does not continue B: another node
	aHandle := engine.Handle(l.RunID, callNamed(t, l, "build:A:executor").ID)
	res := l.Script(`const out = await parallel([async () => { const r = await agent.call('continue', { resume: '`+aHandle+`', node: 'B', label: 'cross' }); return [r.status, r.reason] }], { keys: ['x'] })
return out[0]`, false)
	if got := Value[[]string](t, res); !slices.Equal(got, []string{"refused", "foreign_handle"}) {
		t.Errorf("continuing A's handle for B: %v", got)
	}
}

func str2(m map[string]any, key string) string { return str(m, key) }

// Every decision to spawn or not to spawn is a row of flow_spawn_decisions with the rule that made it; the tier decision is a row
// of flow_step_decisions. The rule ids come from one source and every row names a known one.
func TestSpawnDecisionsRecorded(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "a big issue", Regions: []string{"b/"}, SizeFit: "child", SizeReason: "own_goal"},
	}
	l := NewLab(t, nodes...)
	same := Defect("d1", "correctness", "P1", "the guard is missing")
	reviews(l, "A", Verdict(false, same), Verdict(false, same), Verdict(true))
	d := l.designFor()
	d.OpenQuestions = []ops.OpenQuestion{{ID: "q1", Question: "which flag turns it on?", Scope: "repo", FilesNeeded: []string{"a", "b", "c", "d"}}, {ID: "q2", Question: "is it documented?", Scope: "docs", FilesNeeded: []string{"README.md"}}}
	l.Design = &d
	mustRunIncomplete(t, l)
	type key struct{ rule, decision, node string }
	seen := map[key]int{}
	known := map[string]bool{}
	for _, id := range ops.RuleIDs() {
		known[id] = true
	}
	for _, r := range l.SpawnRows() {
		seen[key{r.Rule, r.Decision, r.Node}]++
		if !known[r.Rule] {
			t.Errorf("a decision under an unknown rule %q", r.Rule)
		}
	}
	for _, k := range []key{
		{ops.RuleArch1, "spawn", ""}, {ops.RuleAudit, "spawn", ""}, {ops.RuleExpl1, "spawn", ""}, {ops.RuleInl1, "inline", ""},
		{ops.RuleExec1, "spawn", "A"}, {ops.RuleRev1, "spawn", "A"}, {ops.RuleExpl3, "skip", "A"}, {ops.RuleChild2, "spawn", "B"},
	} {
		if seen[k] == 0 {
			t.Errorf("no decision %+v among %v", k, seen)
		}
	}
	if seen[key{ops.RuleExec1, "spawn", "A"}] != 2 {
		t.Errorf("SP-EXEC-1 spawns of A: %d, want the executor and the executor-t1 (a fix continues a thread and spawns nothing)", seen[key{ops.RuleExec1, "spawn", "A"}])
	}
	steps := l.StepRows()
	if len(steps) != 1 || steps[0].Rule != ops.RuleTier1 || steps[0].Node != "A" {
		t.Errorf("step rows %+v", steps)
	}
}

// ---- the reviewer is not a verdict ----

func reviewerOnly(t *testing.T, l *Lab, wantLabels []string) {
	t.Helper()
	if labels := l.Labels(); !slices.Equal(labels, wantLabels) {
		t.Fatalf("calls %v, want %v", labels, wantLabels)
	}
	for _, c := range l.Calls() {
		if strings.HasPrefix(c.Label, "fix:") {
			t.Errorf("an executor fix %q after a review that was not a verdict", c.Label)
		}
	}
	noTierStep(t, l)
}

// A reviewer turn the host failed is a held call: the node is held, there is no fix and no tier evidence.
func TestReviewerHeldHoldsNoFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Sequence("^review:A:r1$", func(*Call) fakehostPlan { return Fail("serverOverloaded") })
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "host_infra_serverOverloaded" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "source") != "review" {
		t.Errorf("hold rows %+v", rows)
	}
	if rows := l.Rows("crw.review"); len(rows) != 1 || str(rows[0].Data, "state") != "held" {
		t.Errorf("review rows %+v", rows)
	}
}

// A reviewer call whose outcome is unknown holds the node: nothing is sent again, no fix, no tier evidence.
func TestReviewerUnknownHoldsNoFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Threads.DropAck("turn/start", func(_ int, params map[string]any) bool { return strings.Contains(inputText(params), "Review the head") })
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "dispatch_unknown" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:dispatch_unknown"}) {
		t.Errorf("closes %v", got)
	}
}

// A reviewer that is refused - of the author's own model family (independence_refused), or by the host's configuration -
// escalates as a configuration fault, and nothing is fixed.
func TestReviewerRefusedEscalatesNoFix(t *testing.T) {
	t.Run("independence_refused", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		// the executor runs on the reviewer's model family: its review is no independent second opinion
		spec := l.Policy.Roles["executor"]
		spec.Pair.Model = "gpt-fake-builder"
		l.Policy.Roles["executor"] = spec
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "independence_refused" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
		rows := l.FlowRows(engine.EvEscalation)
		if len(rows) != 1 || str(rows[0].Data, "class") != "configuration_fault" || str(rows[0].Data, "reason") != "independence_refused" {
			t.Errorf("escalations %+v", rows)
		}
		if r := l.Rows("crw.review"); len(r) != 1 || str(r[0].Data, "state") != "refused" {
			t.Errorf("review rows %+v", r)
		}
	})
	t.Run("host configuration", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^review:A:r1$", func(*Call) fakehostPlan {
			return fakehostPlan{StartError: map[string]any{"code": -32600, "message": "bad request", "data": map[string]any{"codexErrorInfo": "badRequest"}}}
		})
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "host_config_badRequest" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1"})
	})
}

// A reviewer that answers null_model is asked once more on a fresh reviewer thread; a second null_model escalates, and a verdict
// of the second thread is the review.
func TestReviewerNullModelRetriesFreshReviewerThenEscalates(t *testing.T) {
	t.Run("escalates", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^review:A:r1", func(*Call) fakehostPlan { return Garbage() })
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "reviewer_null_model" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		reviewerOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "review:A:r1:retry"})
		first, second := callNamed(t, l, "review:A:r1"), callNamed(t, l, "review:A:r1:retry")
		if first.ID == second.ID || second.Resume != "" {
			t.Errorf("the retry is not a fresh reviewer thread: %+v", second)
		}
		if l.Prepared(first.ID).Data["cwd"] == nil {
			t.Log("reviewer calls carry their own directories")
		}
	})
	t.Run("the retry answers", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^review:A:r1$", func(*Call) fakehostPlan { return Garbage() })
		got := mustRun(t, l)
		if got.State != "closed" {
			t.Fatalf("state %q", got.State)
		}
		if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "review:A:r1:retry", "reviewer-final:M1", "reflection:M1"}) {
			t.Fatalf("calls %v", labels)
		}
		noTierStep(t, l)
	})
}

// ---- the worktree of a null_model rebuild ----

// A fix turn that ends null_model leaves its edits in the attempt worktree that is retired; the rebuild starts in a new attempt
// worktree at the node's last commit with that commit's files, and the old worktree's uncommitted edits are only on the retired
// ref. There is no foreign verdict and no hold.
func TestNullModelRebuildStartsFromLastCommitInNewWorktree(t *testing.T) {
	l := NewLab(t, oneNode()...)
	reviews(l, "A", Verdict(false, Defect("d1", "correctness", "P2", "fix this")), Verdict(true))
	l.Sequence("^fix:A:executor:r1$", func(c *Call) fakehostPlan {
		c.EditPath("a/scratch.txt", "an edit the null_model attempt left behind\n")
		return Garbage()
	})
	// what the rebuild finds when its first turn starts: the worktree it was given
	var atStart struct{ head, status, cwd string }
	l.On("^rebuild:A:executor:", func(c *Call) *fakehostPlan {
		atStart.cwd = c.Cwd
		atStart.head = l.GitIn(c.Cwd, "rev-parse", "HEAD")
		atStart.status = l.GitIn(c.Cwd, "status", "--porcelain", "--untracked-files=all")
		return nil
	})
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	labels := l.Labels()
	if !slices.Equal(labels[:6], []string{"architect", "design-audit:r1", "build:A:executor", "review:A:r1", "fix:A:executor:r1", "rebuild:A:executor:r2"}) {
		t.Fatalf("calls %v", labels)
	}
	commits := l.Rows("crw.commit")
	h1 := str(commits[0].Data, "commit")
	rebuild := callNamed(t, l, "rebuild:A:executor:r2")
	alloc, _ := l.Prepared(rebuild.ID).Data["allocation"].(map[string]any)
	if str(alloc, "kind") != "fresh" || !strings.HasSuffix(str(alloc, "path"), "a2-l0") || str(alloc, "start_commit") != h1 {
		t.Fatalf("rebuild allocation %v, want a fresh a2 worktree at %s", alloc, h1)
	}
	// the rebuild began in a2 at HEAD H1 with the files of H1, none of the retired attempt's edits
	if atStart.cwd != str(alloc, "path") || atStart.head != h1 || atStart.status != "" {
		t.Errorf("at the start of the rebuild: worktree %s, HEAD %s, changes %q; want %s at %s with none", atStart.cwd, atStart.head, atStart.status, alloc["path"], h1)
	}
	if _, err := os.Stat(filepath.Join(str(alloc, "path"), "a", "scratch.txt")); err == nil {
		t.Errorf("the retired attempt's edit is in the new worktree")
	}
	if len(commits) != 2 || str(commits[1].Data, "parent") != h1 {
		t.Errorf("the commit after the rebuild is on %v, want H1 %s", commits[len(commits)-1].Data["parent"], h1)
	}
	// the edit survives on the retired ref of attempt 1, on top of its HEAD
	ref := "refs/crw/flow/" + l.RunID + "/retired/A/a1"
	if out := l.Git("show", ref+":a/scratch.txt"); !strings.Contains(out, "left behind") {
		t.Errorf("the retired ref %s holds %q", ref, out)
	}
	if parent := l.Git("rev-parse", ref+"^"); parent != h1 {
		t.Errorf("the retired ref sits on %s, want its HEAD %s", parent, h1)
	}
	if holds := l.Rows("crw.hold"); len(holds) != 0 {
		t.Errorf("hold rows %+v", holds)
	}
	if esc := l.FlowRows(engine.EvEscalation); len(esc) != 0 {
		t.Errorf("escalations %+v", esc)
	}
	if rows := l.FlowRows("worktree_restored"); len(rows) != 0 {
		t.Errorf("a restore for a rebuild: %+v", rows)
	}
	noTierStep(t, l)
}

// The built-in script passes the static rules of `crw flow lint`: the lifecycle of every executor node (build, commit, verify,
// review, land), the tier step before every executor-t1, the reviewer's family, the labels, and the lanes' reads of outer
// variables.
func TestProjectLaneIsLintClean(t *testing.T) {
	text, err := workflows.Source("project-lane")
	if err != nil {
		t.Fatal(err)
	}
	report := lint.Check(context.Background(), script.Source{Name: "project-lane.js", Text: text, Dir: workflows.FS}, lint.Options{})
	if !report.OK() {
		for _, d := range report.Diagnostics {
			t.Errorf("%s: %s: %s (line %d)", report.Script, d.Rule, d.Message, d.Line)
		}
	}
}

// A node the scheduler holds back - no slot is free - is not started and is not given up: the run reports it unreleased, and a
// resume asks the scheduler again. The lane that was released replays and runs nothing.
func TestUnreleasedNodeIsReleasedOnResume(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	l.Relay.Slots = 1
	got := mustRunIncomplete(t, l)
	if got.State != "incomplete" {
		t.Fatalf("state %q: %+v", got.State, got)
	}
	var landed, waiting string
	for _, ln := range got.Lanes {
		switch ln.State {
		case "landed":
			landed = ln.Node
		case "unreleased":
			waiting = ln.Node
			if ln.Reason != "capacity_exhausted" {
				t.Errorf("unreleased for %q", ln.Reason)
			}
		}
	}
	if landed == "" || waiting == "" || landed == waiting {
		t.Fatalf("lanes %+v", got.Lanes)
	}
	if l.Relay.SlotHeld(landed) {
		t.Errorf("the landed node still holds a slot")
	}
	before := l.LastSeq()
	l.NewProcess("proc-2")
	res := l.Resume()
	final := Value[laneResult](t, res)
	if final.State != "closed" {
		t.Fatalf("resume: %+v", final)
	}
	for _, ts := range turnsSince(l, before) {
		// the lane of the node that was waiting, and the close of the milestone it lets go on: the final review
		if label := labelOf(l, ts.call); !strings.Contains(label, ":"+waiting+":") && label != "reviewer-final:M1" && label != "reflection:M1" {
			t.Errorf("the resume ran %q: only the lane of the node that was waiting runs, and the close after it", label)
		}
	}
	if !slices.Equal(l.Relay.Landed, []string{landed, waiting}) {
		t.Errorf("landed %v", l.Relay.Landed)
	}
}

// A verification that could not run (the verifier is not installed, it left no record) is not a failed verification: the node is
// held, nothing is fixed, no reviewer is called and no tier evidence is counted.
func TestVerificationThatCannotRunHoldsNoTierStep(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Unavailable = map[string]bool{"A": true}
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "verify_unavailable" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor"}) {
		t.Fatalf("calls %v", labels)
	}
	if rows := l.Rows("crw.verify"); len(rows) != 1 || rows[0].Data["ran"] != false || rows[0].Data["ok"] != false {
		t.Errorf("verify rows %+v", rows)
	}
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "reason") != "verify_unavailable" {
		t.Errorf("hold rows %+v", rows)
	}
	noTierStep(t, l)
}

// An executor that keeps asking for research gets two continuations on the tier: the third question is not sent to a third
// explorer, and the node escalates as a scope question.
func TestResearchCapEscalatesAsScopeQuestion(t *testing.T) {
	l := NewLab(t, oneNode()...)
	asks := 0
	l.On("^(build|research):A:executor", func(c *Call) *fakehostPlan {
		asks++
		p := Say(ReceiptWith("needs_research", map[string]any{"research_question": fmt.Sprintf("which module owns case %d?", asks)}))
		return &p
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "blocked_scope" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if n := len(l.CallsOf("explorer")); n != 2 {
		t.Errorf("%d explorer calls, want the two continuations of the tier", n)
	}
	if n := len(l.CallsOf("executor")); n != 3 {
		t.Errorf("%d executor calls (the build and two research continuations)", n)
	}
	esc := l.FlowRows(engine.EvEscalation)
	if len(esc) != 1 || str(esc[0].Data, "detail") != "research_cap" || str(esc[0].Data, "class") != "scope" {
		t.Errorf("escalations %+v", esc)
	}
	if got := l.Relay.CloseReasons(); !slices.Equal(got, []string{"A:blocked_scope"}) {
		t.Errorf("closes %v", got)
	}
	noTierStep(t, l)
}

// The verification of a head names the range the acceptance judges: its base is the tip of the integration target the node started
// from, so the verification record and the premerge record of the node talk about the same commits.
func TestVerifyNamesTheIntegrationTipAsBase(t *testing.T) {
	l := NewLab(t, oneNode()...)
	start := l.Tip()
	mustRun(t, l)
	if len(l.Verify.Calls) == 0 {
		t.Fatal("nothing was verified")
	}
	for _, c := range l.Verify.Calls {
		if c.Base != start {
			t.Errorf("verification of %s at %s has base %q, want the tip %s", c.Node, c.Head, c.Base, start)
		}
	}
}
