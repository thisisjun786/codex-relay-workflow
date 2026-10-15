package scenario

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/lint"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dagsched"
)

// A node the architect sized for a child goes up as a size split (CRW-1415): the issue-child path is retired, so the lane escalates it
// with reason size_split and detail child_retired, starts no child and calls no executor for it, closes its execution so no slot
// stays held, and the other nodes of the milestone are not held up by it.
func TestSizeFitChildEscalatesSizeSplitAndStartsNoChild(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "a big issue", Regions: []string{"a/"}, SizeFit: "child", SizeReason: "regions_gt3"},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "size_split" {
		t.Fatalf("lane A: %s %s, want escalated size_split", state, reason)
	}
	if state, _ := laneOf(t, got, "B"); state != "landed" {
		t.Fatalf("lane B: %s", state)
	}
	var esc []engine.Event
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		if str(ev.Data, "node") == "A" {
			esc = append(esc, ev)
		}
	}
	if len(esc) != 1 || esc[0].Op != ops.OpEscalate || str(esc[0].Data, "reason") != "size_split" || str(esc[0].Data, "detail") != "child_retired" {
		t.Fatalf("escalations of A %+v, want one size_split with detail child_retired", esc)
	}
	if ev, _ := esc[0].Data["evidence"].(map[string]any); str(ev, "size_reason") != "regions_gt3" {
		t.Errorf("the escalation of A carries evidence %v, want the size reason", esc[0].Data["evidence"])
	}
	if rows := l.Rows(ops.OpReleaseChild); len(rows) != 0 {
		t.Errorf("a crw.releaseChild row: %+v", rows)
	}
	for _, r := range l.SpawnRows() {
		if r.Rule == ops.RuleChild1 || r.Rule == ops.RuleChild2 {
			t.Errorf("a decision under a retired child rule: %+v", r)
		}
		if r.Node == "A" && r.Rule == ops.RuleExec1 {
			t.Errorf("an executor spawn for the child-sized node: %+v", r)
		}
	}
	for _, c := range l.Calls() {
		if c.Node == "A" {
			t.Errorf("a call for the child-sized node: %+v", c)
		}
	}
	if !slices.Contains(l.Relay.CloseReasons(), "A:size_split") {
		t.Errorf("closes %v: A's execution was not closed", l.Relay.CloseReasons())
	}
	if l.Relay.SlotHeld("A") {
		t.Errorf("a slot is held for the child-sized node")
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
	d.OpenQuestions = []ops.OpenQuestion{{ID: "q1", Question: "which flag turns it on?", Scope: "repo", FilesNeeded: []string{"a", "b", "c", "d"}}, {ID: "q2", Question: "is it documented?", Scope: "docs", FilesNeeded: []string{"README.md"}},
		{ID: "q3", Question: "may it skip the README rule?", Scope: "authority:repo-rule", FilesNeeded: []string{"README.md"}}}
	l.Design = &d
	got := mustRunIncomplete(t, l)
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
		{ops.RuleArch1, "spawn", ""}, {ops.RuleAudit, "spawn", ""}, {ops.RuleExpl1, "spawn", ""}, {ops.RuleExpl1, "orchestrator", ""},
		{ops.RuleExec1, "spawn", "A"}, {ops.RuleRev1, "spawn", "A"}, {ops.RuleExpl3, "skip", "A"},
	} {
		if seen[k] == 0 {
			t.Errorf("no decision %+v among %v", k, seen)
		}
	}
	// B, sized for a child, goes up as a size split (CRW-1415): no decision under a retired child rule is written for it
	for k := range seen {
		if k.node == "B" && (k.rule == ops.RuleChild1 || k.rule == ops.RuleChild2) {
			t.Errorf("a decision %+v under a retired child rule", k)
		}
	}
	if state, reason := laneOf(t, got, "B"); state != "escalated" || reason != "size_split" {
		t.Errorf("lane B: %s %s, want escalated size_split", state, reason)
	}
	// an open question of a DESIGN is never the asker's (CRW-1347): the one with one file is the second explorer
	if seen[key{ops.RuleExpl1, "spawn", ""}] != 2 || seen[key{ops.RuleInl1, "inline", ""}] != 0 {
		t.Errorf("SP-EXPL-1 spawns %d, SP-INL-1 rows %d: want two explorers and no inline route", seen[key{ops.RuleExpl1, "spawn", ""}], seen[key{ops.RuleInl1, "inline", ""}])
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

// The lane admits two nodes at once only when the relay's own region rule lets both be released (CRW-1461): crw.dag reads the overlap
// of two nodes from crw.regionsOverlap, which judges by the relay's rule (dagsched.Overlaps over the normal forms, with the plan's change
// and hold), so a pair the relay would hold back defer:edit_overlap is never admitted together. Each case is a pair A, B the old
// script-side comparison of texts took for disjoint - another spelling of one file, a symbol of a file the other edits whole, a symbol
// of a file the other deletes a symbol of - beside a node C on a file of its own: with two slots, A and C run at once, B is asked for
// only after A landed, and no release is held back.
func TestDagAdmitsByTheRelayRule(t *testing.T) {
	for _, tc := range []struct {
		name    string
		a, b    string
		planned []ops.LaneRegion
	}{
		{"another spelling", "a.go", "./a.go", nil},
		{"a symbol of the file", "a.go", "a.go#F", nil},
		{"a symbol of a file a delete makes one place", "pkg/a.go#F", "pkg/a.go#G", []ops.LaneRegion{{Text: "pkg/a.go#F", Change: "delete"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := NewLab(t,
				NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{tc.a}, Planned: tc.planned},
				NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{tc.b}},
				NodeSpec{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"c.go"}},
			)
			l.Cap = 2
			l.Relay.HoldBackIf = l.Relay.relayRegionRule
			// C's release is answered before A integrates: C runs beside A, and what B waits for is A's landing, not C's
			cReleased := make(chan struct{})
			var once sync.Once
			l.OnRow(func(ev engine.Event) {
				if ev.Kind == engine.KindCall && ev.Op == ops.OpRelease && ev.Event == engine.EvCompleted && str(ev.Data, "node") == "C" {
					once.Do(func() { close(cReleased) })
				}
			})
			var mu sync.Mutex
			startSeq := map[string][]int{}
			l.Relay.OnRelease = func(req ops.ReleaseRequest) {
				seq := l.LastSeq()
				mu.Lock()
				startSeq[req.Node] = append(startSeq[req.Node], seq)
				mu.Unlock()
			}
			l.Relay.OnIntegrate = func(node string) {
				if node != "A" {
					return
				}
				select {
				case <-cReleased:
				case <-time.After(2 * time.Minute):
					t.Errorf("A integrated before C's release was answered")
				}
			}
			got := mustRunIncomplete(t, l)
			for _, ev := range l.Rows(ops.OpRelease) {
				if str(ev.Data, "reason") == "defer:edit_overlap" {
					t.Errorf("the release of %s was held back defer:edit_overlap", str(ev.Data, "node"))
				}
			}
			if got.State != "closed" {
				t.Fatalf("state %q, lanes %+v; calls %v", got.State, got.Lanes, l.Labels())
			}
			for _, n := range []string{"A", "B", "C"} {
				if state, reason := laneOf(t, got, n); state != "landed" {
					t.Errorf("lane %s: %s %s, want landed", n, state, reason)
				}
			}
			seqOfRow := func(op, node string) int {
				for _, ev := range l.Rows(op) {
					if str(ev.Data, "node") == node {
						return ev.Seq
					}
				}
				t.Fatalf("no completed %s row of %s", op, node)
				return 0
			}
			integratedA := seqOfRow(ops.OpIntegrate, "A")
			releasedC := seqOfRow(ops.OpRelease, "C")
			mu.Lock()
			asksB := append([]int(nil), startSeq["B"]...)
			mu.Unlock()
			if len(asksB) == 0 {
				t.Fatalf("B was never asked for")
			}
			if integratedA > asksB[0] {
				t.Errorf("B was first asked for at seq %d, before A integrated at seq %d", asksB[0], integratedA)
			}
			if releasedC >= integratedA {
				t.Errorf("C was released at seq %d, not before A integrated at seq %d: C did not run beside A", releasedC, integratedA)
			}
		})
	}
}

// ---- a release held back by the run's own node (CRW-1467) ----

// releaseOf says whether ev is a completed crw.release row of node.
func releaseOf(ev engine.Event, node string) bool {
	return ev.Kind == engine.KindCall && ev.Op == ops.OpRelease && ev.Event == engine.EvCompleted && str(ev.Data, "node") == node
}

// heldBack says whether ev is a completed crw.release row of node that the relay held back for an edit overlap.
func heldBack(ev engine.Event, node string) bool {
	return releaseOf(ev, node) && !flagOf(ev.Data, "released") && str(ev.Data, "reason") == dagsched.DeferEditOverlap
}

// releasesOf are the completed crw.release rows of node, in the order of the journal.
func releasesOf(l *Lab, node string) []engine.Event {
	var out []engine.Event
	for _, ev := range l.Events() {
		if releaseOf(ev, node) {
			out = append(out, ev)
		}
	}
	return out
}

// deferWhileHeld is a relay that holds node back defer:edit_overlap while holder holds its regions (released, not yet integrated or
// closed): the relay's judgment of two nodes the lane admitted together, which the lane cannot see.
func deferWhileHeld(node, holder string) func(ops.ReleaseRequest, map[string][]ops.LaneRegion) string {
	return func(req ops.ReleaseRequest, holding map[string][]ops.LaneRegion) string {
		if _, held := holding[holder]; req.Node == node && held {
			return dagsched.DeferEditOverlap
		}
		return ""
	}
}

// waitFor waits until ch is closed, failing the test when it is not within two minutes.
func waitFor(t *testing.T, ch <-chan struct{}, what string) {
	select {
	case <-ch:
	case <-time.After(2 * time.Minute):
		t.Errorf("timed out waiting for %s", what)
	}
}

// firstRowSeq is the Seq of the first completed row of op for node.
func firstRowSeq(t *testing.T, l *Lab, op, node string) int {
	t.Helper()
	for _, ev := range l.Rows(op) {
		if str(ev.Data, "node") == node {
			return ev.Seq
		}
	}
	t.Fatalf("no completed %s row of %s", op, node)
	return 0
}

// resumeCompletedZeroLive resumes the completed run of l in a new process: the milestone closes at the same tip, no call runs live,
// no operation but crw.classify runs live and nothing is superseded.
func resumeCompletedZeroLive(t *testing.T, l *Lab) {
	t.Helper()
	tip := l.Tip()
	before := l.LastSeq()
	l.NewProcess("proc-2")
	got := Value[laneResult](t, l.Resume())
	if got.State != "closed" || got.Tip != tip {
		t.Fatalf("resume: %+v, want closed at %s", got, tip)
	}
	if live := l.LiveCalls(before); len(live) != 0 {
		t.Errorf("the resume made %d live calls: %+v", len(live), live)
	}
	for _, ev := range rowsAfter(l, before) {
		if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Op, "crw.") && ev.Op != "crw.classify" {
			t.Errorf("an operation ran live: %s at %s", ev.Op, ev.Path)
		}
		if ev.Event == engine.EvSuperseded {
			t.Errorf("a call was superseded: %s %s", ev.Label, ev.Path)
		}
	}
}

// checkDeferredThenReleased says that B was held back once, then released, and landed beside A in a milestone that closed.
func checkDeferredThenReleased(t *testing.T, l *Lab, got laneResult) {
	t.Helper()
	if got.State != "closed" {
		t.Fatalf("state %q, lanes %+v; calls %v", got.State, got.Lanes, l.Labels())
	}
	for _, n := range []string{"A", "B"} {
		if state, reason := laneOf(t, got, n); state != "landed" {
			t.Errorf("lane %s: %s %s, want landed", n, state, reason)
		}
	}
	rows := releasesOf(l, "B")
	if len(rows) != 2 || !heldBack(rows[0], "B") || !flagOf(rows[1].Data, "released") {
		t.Fatalf("B's release rows %+v, want one held back defer:edit_overlap and then one released", rows)
	}
}

// A release the relay held back defer:edit_overlap while another node of the same dag held its regions is the run's own doing: the node
// is put back to pending, it holds no slot of the lane's cap and no regions while it waits, and it is admitted again once another node of
// the dag has ended, so it is asked for again after the node that held it back landed (CRW-1467). The order is fixed: B is first asked
// for after A was released, and A integrates only after the lane script has taken B's held-back answer (B is pending then). A resume of the
// completed run replays it all: the held-back answer is one the lane asked again after (crw.deferredAsks). A node the relay holds back
// while no other node of its dag runs, or has ended since it asked, is held back by something else: it ends unreleased, as before.
func TestSelfCausedEditOverlapDeferReadmitsAfterTheOtherNodeLands(t *testing.T) {
	t.Run("another node of the dag holds", func(t *testing.T) {
		l := NewLab(t,
			NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}},
			NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b.go"}},
		)
		l.Cap = 2
		l.Relay.HoldBackIf = deferWhileHeld("B", "A")
		aReleased, bPending := make(chan struct{}), make(chan struct{})
		var aOnce, bOnce sync.Once
		l.OnRow(func(ev engine.Event) {
			if releaseOf(ev, "A") && flagOf(ev.Data, "released") {
				aOnce.Do(func() { close(aReleased) })
			}
		})
		l.OnSettled(func(ev engine.Event) {
			if heldBack(ev, "B") {
				bOnce.Do(func() { close(bPending) })
			}
		})
		var mu sync.Mutex
		startSeq := map[string][]int{}
		l.Relay.OnRelease = func(req ops.ReleaseRequest) {
			seq := l.LastSeq()
			mu.Lock()
			startSeq[req.Node] = append(startSeq[req.Node], seq)
			first := len(startSeq[req.Node]) == 1
			mu.Unlock()
			if req.Node == "B" && first {
				waitFor(t, aReleased, "A's release")
			}
		}
		l.Relay.OnIntegrate = func(node string) {
			if node == "A" {
				waitFor(t, bPending, "the lane to take B's held-back answer")
			}
		}
		got := mustRunIncomplete(t, l)
		checkDeferredThenReleased(t, l, got)
		rows := releasesOf(l, "B")
		integratedA := firstRowSeq(t, l, ops.OpIntegrate, "A")
		mu.Lock()
		asksB := append([]int(nil), startSeq["B"]...)
		mu.Unlock()
		if len(asksB) != 2 {
			t.Fatalf("B was asked for %d times (%v), want twice", len(asksB), asksB)
		}
		if !(rows[0].Seq < integratedA && integratedA <= asksB[1]) {
			t.Errorf("B held back at seq %d, A integrated at %d, B asked again at %d: want B asked again after A landed", rows[0].Seq, integratedA, asksB[1])
		}
		resumeCompletedZeroLive(t, l)
	})
	// the plan changes while B waits: its next read of the plan is a trigger, and it is released under the revision the signal made. A
	// resume replays B's held-back answer before its claim waits for that signal (the signal waits for the read after the answer)
	t.Run("a plan revision between the asks", func(t *testing.T) {
		l := NewLab(t,
			NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}},
			NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b.go"}},
		)
		l.Cap = 2
		l.Relay.HoldBackIf = deferWhileHeld("B", "A")
		aReleased, bPending := make(chan struct{}), make(chan struct{})
		var aOnce, bOnce sync.Once
		l.OnRow(func(ev engine.Event) {
			if releaseOf(ev, "A") && flagOf(ev.Data, "released") {
				aOnce.Do(func() { close(aReleased) })
			}
		})
		l.OnSettled(func(ev engine.Event) {
			if heldBack(ev, "B") {
				bOnce.Do(func() { close(bPending) })
			}
		})
		var asks atomic.Int32
		l.Relay.OnRelease = func(req ops.ReleaseRequest) {
			if req.Node == "B" && asks.Add(1) == 1 {
				waitFor(t, aReleased, "A's release")
			}
		}
		l.Relay.OnIntegrate = func(node string) {
			if node == "A" {
				waitFor(t, bPending, "the lane to take B's held-back answer")
				l.Relay.ChangeNode("B", func(n *NodeSpec) { n.Regions = append(slices.Clone(n.Regions), "b-more/") })
			}
		}
		got := mustRun(t, l)
		checkDeferredThenReleased(t, l, got)
		if rev := releaseRevisions(l); rev["B"] != "r1" {
			t.Fatalf("release revisions %v: B is released under the revision the plan change made", rev)
		}
		resumeCompletedZeroLive(t, l)
	})
	// C overlaps B, so it is admitted only while B is pending; its RECEIPTs make a re-plan, and B is released under that revision after C
	// landed. A resume replays B's held-back answer and puts B back to pending, so C runs (and its signal replays) before B claims again
	t.Run("the node that ran in its place made a revision", func(t *testing.T) {
		l := NewLab(t,
			NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}},
			NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b.go"}},
			NodeSpec{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"b.go#C"}},
		)
		l.Cap = 2
		mismatching(l, "C")
		fixedOnce(l, "C")
		l.Relay.HoldBackIf = deferWhileHeld("B", "A")
		aReleased, bPending := make(chan struct{}), make(chan struct{})
		var aOnce, bOnce sync.Once
		l.OnRow(func(ev engine.Event) {
			if releaseOf(ev, "A") && flagOf(ev.Data, "released") {
				aOnce.Do(func() { close(aReleased) })
			}
		})
		l.OnSettled(func(ev engine.Event) {
			if heldBack(ev, "B") {
				bOnce.Do(func() { close(bPending) })
			}
		})
		var asks atomic.Int32
		l.Relay.OnRelease = func(req ops.ReleaseRequest) {
			if req.Node == "B" && asks.Add(1) == 1 {
				waitFor(t, aReleased, "A's release")
			}
		}
		l.Relay.OnIntegrate = func(node string) {
			if node == "A" {
				waitFor(t, bPending, "the lane to take B's held-back answer")
			}
		}
		got := mustRun(t, l)
		checkDeferredThenReleased(t, l, got)
		if rev := releaseRevisions(l); rev["A"] != "r0" || rev["C"] != "r0" || rev["B"] != "r1" {
			t.Fatalf("release revisions %v: B is released under the revision C's signal made", rev)
		}
		if i := slices.Index(l.Relay.Landed, "B"); i < 0 || i < slices.Index(l.Relay.Landed, "C") {
			t.Errorf("landed %v: B, which overlaps C, lands after it", l.Relay.Landed)
		}
		resumeCompletedZeroLive(t, l)
	})
	t.Run("no other node of the dag", func(t *testing.T) {
		l := NewLab(t, NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}})
		l.Relay.HoldBackIf = func(ops.ReleaseRequest, map[string][]ops.LaneRegion) string { return dagsched.DeferEditOverlap }
		got := mustRunIncomplete(t, l)
		if got.State != "incomplete" {
			t.Fatalf("state %q, lanes %+v", got.State, got.Lanes)
		}
		if state, reason := laneOf(t, got, "A"); state != "unreleased" || reason != dagsched.DeferEditOverlap {
			t.Errorf("lane A: %s %s, want unreleased %s", state, reason, dagsched.DeferEditOverlap)
		}
		if rows := releasesOf(l, "A"); len(rows) != 1 {
			t.Errorf("A's release rows %+v, want the one held back", rows)
		}
	})
}

// The node the relay held back is re-admitted when another node of the dag ends after the answer, also when that node ended before the
// lane script took the answer: the count of ended nodes the node compares with is the one of its ask, not of the moment it reads the
// answer. Here the row of B's held-back answer is written, and the lane takes it only after A's lane has run to its end (A integrated,
// and the script went on from that answer): no other node runs when B reads its answer, and B is asked for again all the same.
func TestSelfCausedDeferReadmitsWhenTheOtherNodeEndsBeforeTheAnswerIsProcessed(t *testing.T) {
	l := NewLab(t,
		NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}},
		NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b.go"}},
	)
	l.Cap = 2
	l.Relay.HoldBackIf = deferWhileHeld("B", "A")
	aReleased, bWritten, aEnded := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var aOnce, bOnce, endOnce sync.Once
	var mu sync.Mutex
	var order []string
	l.OnRow(func(ev engine.Event) {
		if releaseOf(ev, "A") && flagOf(ev.Data, "released") {
			aOnce.Do(func() { close(aReleased) })
		}
		// the answer is written; the lane gets it only once A's lane has ended
		if heldBack(ev, "B") {
			first := false
			bOnce.Do(func() { first = true; close(bWritten) })
			if first {
				waitFor(t, aEnded, "A's lane to end")
			}
		}
	})
	l.OnSettled(func(ev engine.Event) {
		switch {
		case ev.Op == ops.OpIntegrate && str(ev.Data, "node") == "A":
			mu.Lock()
			order = append(order, "A integrated")
			mu.Unlock()
			endOnce.Do(func() { close(aEnded) })
		case heldBack(ev, "B"):
			mu.Lock()
			order = append(order, "B held back")
			mu.Unlock()
		}
	})
	asks := 0
	l.Relay.OnRelease = func(req ops.ReleaseRequest) {
		if req.Node != "B" {
			return
		}
		mu.Lock()
		asks++
		first := asks == 1
		mu.Unlock()
		if first {
			waitFor(t, aReleased, "A's release")
		}
	}
	l.Relay.OnIntegrate = func(node string) {
		if node == "A" {
			waitFor(t, bWritten, "B's held-back answer")
		}
	}
	got := mustRunIncomplete(t, l)
	checkDeferredThenReleased(t, l, got)
	mu.Lock()
	seen := append([]string(nil), order...)
	mu.Unlock()
	if len(seen) < 2 || seen[0] != "A integrated" || seen[1] != "B held back" {
		t.Errorf("the script went on from %v, want A's integration before B's held-back answer", seen)
	}
	resumeCompletedZeroLive(t, l)
}

// A node that waits in its claim for the regions of a node that the relay then holds back is woken: B is running and has not claimed
// when a signal's refresh gives it regions that overlap A's, which A holds as it asks for its release, so B waits in its claim. The relay
// holds A back; A gives up its claim and goes pending, and B, woken, claims and runs. A is admitted again after B ended. Both land.
func TestSelfCausedDeferWakesTheNodeWaitingInClaim(t *testing.T) {
	l := NewLab(t,
		NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}},
		NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	)
	l.Cap = 2
	// the re-plan of the plan revision gives B A's directory
	l.On("^replan:", func(*Call) *fakehostPlan {
		d := l.designFor()
		for i := range d.Issues {
			if d.Issues[i].Node == "B" {
				d.Issues[i].EditRegions = []string{"a/"}
			}
		}
		p := Say(d)
		return &p
	})
	var aAsks atomic.Int32
	changed, bWaits := make(chan struct{}), make(chan struct{})
	var waitOnce sync.Once
	var doneSeq atomic.Int64
	l.OnRow(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Op == "crw.signalDone" && ev.Event == engine.EvCompleted && doneSeq.Load() == 0 {
			doneSeq.Store(int64(ev.Seq))
		}
	})
	// the judgment of B's new regions comes after the signal's done row; when the script went on from it, the refresh gave B its regions,
	// the signal is decided and B, which overlaps A now, waits in its claim
	l.OnSettled(func(ev engine.Event) {
		if ev.Op == "crw.regionsOverlap" && ev.Event == engine.EvCompleted && doneSeq.Load() > 0 && int64(ev.Seq) > doneSeq.Load() {
			waitOnce.Do(func() { close(bWaits) })
		}
	})
	l.Relay.OnRelease = func(req ops.ReleaseRequest) {
		switch req.Node {
		case "A":
			if aAsks.Add(1) == 1 {
				// A claimed and is asking: the plan changes B's slice, so B is not released and reads the plan again (a trigger)
				l.Relay.ChangeNode("B", func(n *NodeSpec) { n.Regions = append(slices.Clone(n.Regions), "b-more/") })
				close(changed)
				waitFor(t, bWaits, "B to wait in its claim")
			}
		case "B":
			waitFor(t, changed, "the plan change")
		}
	}
	l.Relay.HoldBackIf = func(req ops.ReleaseRequest, _ map[string][]ops.LaneRegion) string {
		if req.Node == "A" && aAsks.Load() == 1 {
			return dagsched.DeferEditOverlap
		}
		return ""
	}
	got := mustRun(t, l)
	if got.State != "closed" {
		t.Fatalf("state %q, lanes %+v; calls %v; escalations %v", got.State, got.Lanes, l.Labels(), escalationReasons(l))
	}
	for _, n := range []string{"A", "B"} {
		if state, reason := laneOf(t, got, n); state != "landed" {
			t.Errorf("lane %s: %s %s, want landed", n, state, reason)
		}
	}
	rows := releasesOf(l, "A")
	if len(rows) != 2 || !heldBack(rows[0], "A") || !flagOf(rows[1].Data, "released") {
		t.Fatalf("A's release rows %+v, want one held back and then one released", rows)
	}
	if !slices.Equal(l.Relay.Landed, []string{"B", "A"}) {
		t.Errorf("landed %v, want B, then A", l.Relay.Landed)
	}
}

// A release held back twice and asked again after each answer is proven up to the last ask: a resume replays both held-back answers
// in the order the record has them (crw.deferredAsks), whatever the order the other lanes replay in, and asks the relay once more, live.
// First process: A, C and B run at once (cap 3); the relay holds B back while A holds. B's first answer comes while A and C run (B goes
// pending), C lands (B is asked again), the second answer comes while A still holds (B goes pending again), A lands (B is asked a third
// time), and the process dies inside that third relay call, which leaves no row. Second process: the replays of C's and A's integrations
// and of B's first plan read are held, then settled in that order, so both other lanes have ended before B asks for its first release:
// no other node runs and none ended since the ask, and only the record says that B was asked again after that answer.
func TestTwiceDeferredReleaseIsAskedAgainOnResume(t *testing.T) {
	l := NewLab(t,
		NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}},
		NodeSpec{ID: "C", Issue: "CRW-C", Title: "issue C", Regions: []string{"c.go"}},
		NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b.go"}},
	)
	l.Cap = 3
	l.Relay.HoldBackIf = deferWhileHeld("B", "A")
	aReleased := make(chan struct{})
	pending := []chan struct{}{make(chan struct{}), make(chan struct{})}
	var aOnce sync.Once
	var mu sync.Mutex
	taken := 0
	l.OnRow(func(ev engine.Event) {
		if releaseOf(ev, "A") && flagOf(ev.Data, "released") {
			aOnce.Do(func() { close(aReleased) })
		}
	})
	l.OnSettled(func(ev engine.Event) {
		if !heldBack(ev, "B") {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if taken < len(pending) {
			close(pending[taken])
		}
		taken++
	})
	asks := 0
	killed := false
	l.Relay.OnRelease = func(req ops.ReleaseRequest) {
		if req.Node != "B" {
			return
		}
		mu.Lock()
		asks++
		first := asks == 1
		mu.Unlock()
		if first {
			waitFor(t, aReleased, "A's release")
		}
	}
	l.Relay.ReleaseErr = func(node string) error {
		mu.Lock()
		third := node == "B" && asks == 3 && !killed
		if third {
			killed = true
		}
		mu.Unlock()
		if !third {
			return nil
		}
		l.Kill()
		return fmt.Errorf("the process died inside the relay call")
	}
	l.Relay.OnIntegrate = func(node string) {
		switch node {
		case "C":
			waitFor(t, pending[0], "the lane to take B's first held-back answer")
		case "A":
			waitFor(t, pending[1], "the lane to take B's second held-back answer")
		}
	}
	l.Run()
	mu.Lock()
	wasKilled, asked := killed, asks
	mu.Unlock()
	if !wasKilled {
		t.Fatalf("the process was not killed in B's third relay call; B asked %d times; calls %v", asked, l.Labels())
	}
	// what the first process left
	pathOf := map[string]string{}
	posOf := map[string]int{}
	for _, n := range []string{"A", "C"} {
		for _, ev := range l.Rows(ops.OpIntegrate) {
			if str(ev.Data, "node") == n {
				pathOf[n], posOf[n] = ev.Path, ev.Position
			}
		}
		if pathOf[n] == "" {
			t.Fatalf("%s has no crw.integrate row", n)
		}
	}
	rows := releasesOf(l, "B")
	if len(rows) != 2 || !heldBack(rows[0], "B") || !heldBack(rows[1], "B") {
		t.Fatalf("B's release rows %+v, want two held back defer:edit_overlap", rows)
	}
	for _, ev := range l.Events() {
		if ev.Kind == engine.KindCall && ev.Op == ops.OpRelease && ev.Path == rows[1].Path && ev.Position > rows[1].Position {
			t.Errorf("a crw.release row of B after its second: %+v", ev)
		}
		for _, n := range []string{"A", "C"} {
			if ev.Path == pathOf[n] && ev.Position > posOf[n] {
				t.Errorf("a row of %s's path after its integration: %s %s at %d", n, ev.Op, ev.Event, ev.Position)
			}
		}
	}
	firstB := rows[0].CallID

	// the second process: the other lanes end before B asks for its first release
	before := l.LastSeq()
	l.Relay.mu.Lock()
	mark := len(l.Relay.Releases)
	l.Relay.mu.Unlock()
	l.NewProcess("proc-2")
	var hmu sync.Mutex
	held := map[string]func(){}
	allHeld := make(chan struct{})
	l.ReplayHold = func(row engine.Event, settle func()) bool {
		key := ""
		switch {
		case row.Op == ops.OpIntegrate && (str(row.Data, "node") == "A" || str(row.Data, "node") == "C"):
			key = str(row.Data, "node")
		case row.Op == "crw.planRead" && str(row.Data, "node") == "B":
			key = "B"
		}
		hmu.Lock()
		defer hmu.Unlock()
		if key == "" || held[key] != nil {
			return false
		}
		held[key] = settle
		if len(held) == 3 {
			close(allHeld)
		}
		return true
	}
	go func() {
		select {
		case <-allHeld:
		case <-time.After(2 * time.Minute):
			t.Errorf("the replays of C's and A's integrations and of B's plan read were not all held")
			return
		}
		hmu.Lock()
		defer hmu.Unlock()
		for _, k := range []string{"C", "A", "B"} {
			held[k]()
		}
	}()
	res := l.Resume()
	final := Value[laneResult](t, res)
	if final.State != "closed" {
		t.Fatalf("resume: %+v; calls %v", final, l.Labels())
	}
	if state, reason := laneOf(t, final, "B"); state != "landed" {
		t.Errorf("lane B: %s %s, want landed", state, reason)
	}
	l.Relay.mu.Lock()
	var live []ops.ReleaseRequest
	for _, r := range l.Relay.Releases[mark:] {
		if r.Node == "B" {
			live = append(live, r)
		}
	}
	l.Relay.mu.Unlock()
	if len(live) != 1 {
		t.Errorf("B's release was asked of the relay %d times in the resume, want once", len(live))
	}
	if after := releasesOf(l, "B"); len(after) == 0 || !flagOf(after[len(after)-1].Data, "released") {
		t.Errorf("B's last release row %+v is no release", after)
	}
	superseded := supersededCalls(l, before)
	if superseded[firstB] {
		t.Errorf("B's first held-back release was superseded")
	}
	for _, ev := range rowsAfter(l, before) {
		for _, n := range []string{"A", "C"} {
			if ev.Path != pathOf[n] {
				continue
			}
			if ev.Event == engine.EvSuperseded {
				t.Errorf("a row of %s's path was superseded: %s %s", n, ev.Op, ev.Label)
			}
			if ev.Kind == engine.KindCall && ev.Event == engine.EvCompleted && strings.HasPrefix(ev.Op, "crw.") && ev.Op != "crw.classify" {
				t.Errorf("an operation of %s ran live: %s", n, ev.Op)
			}
		}
	}
	for _, c := range l.LiveCalls(before) {
		if strings.Contains(c.Label, ":A:") || strings.Contains(c.Label, ":C:") {
			t.Errorf("the resume ran %q live", c.Label)
		}
	}
}
