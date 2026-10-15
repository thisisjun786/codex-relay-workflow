package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/lint"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
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
	// two unit checks in a row find the same finding: SP-TIER-1 rule (a) steps A's tier
	same := Finding("P1", "the guard is missing")
	l.UnitChecks("A", NeedsChanges(same), NeedsChanges(same))
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
	same := Finding("P1", "the guard is missing")
	l.UnitChecks("A", NeedsChanges(same), NeedsChanges(same))
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
		{ops.RuleExec1, "spawn", "A"},
	} {
		if seen[k] == 0 {
			t.Errorf("no decision %+v among %v", k, seen)
		}
	}
	// the unit check of a node is a run of the review tool, not a reviewer spawn (CRW-1465): no SP-REV-1 decision, and no evidence
	// question of a reviewer to route (SP-EXPL-3)
	for k := range seen {
		if k.node == "A" && (k.rule == ops.RuleRev1 || k.rule == ops.RuleExpl3) {
			t.Errorf("a decision %+v of the old reviewer path", k)
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

// ---- a unit check that is not a verdict ----

func checkOnly(t *testing.T, l *Lab, wantLabels []string) {
	t.Helper()
	if labels := l.Labels(); !slices.Equal(labels, wantLabels) {
		t.Fatalf("calls %v, want %v", labels, wantLabels)
	}
	for _, c := range l.Calls() {
		if strings.HasPrefix(c.Label, "fix:") || c.Role == "reviewer" {
			t.Errorf("a call %q (%s) after a unit check that was not a verdict", c.Label, c.Role)
		}
	}
	if n := len(l.Rows("crw.commit")); n != 0 {
		t.Errorf("%d commits after a unit check that was not a verdict", n)
	}
	noTierStep(t, l)
}

// A unit check the tool gave no answer in (CRW-1465) holds the node: the hold's source is unit_check, there is no fix, no commit and no
// tier evidence.
func TestUnitCheckNoAnswerHoldsNoFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.UnitChecks("A", UnitCheck{State: reviewadapter.StateNoAnswer})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "unit_check_no_answer" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	checkOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor"})
	if rows := l.Rows("crw.hold"); len(rows) != 1 || str(rows[0].Data, "source") != "unit_check" {
		t.Errorf("hold rows %+v", rows)
	}
	if rows := l.Rows(ops.OpUnitCheck); len(rows) != 1 || str(rows[0].Data, "hold") != "unit_check_no_answer" || str(rows[0].Data, "role") != "unit-check" {
		t.Errorf("unit-check rows %+v", rows)
	}
}

// A worktree that moved after the executor's turn (CRW-1465) is not checked: the tool is not run on a tree the run will not commit,
// and the node escalates as a configuration question (unit_check_tree_moved); nothing is fixed and nothing is committed.
func TestUnitCheckOfAMovedTreeEscalatesNoFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	var moved sync.Once
	l.OnRow(func(ev engine.Event) {
		if ev.Kind == engine.KindCall && ev.Op == "agent" && ev.Event == engine.EvCompleted && ev.Label == "build:A:executor" {
			moved.Do(func() {
				alloc, _ := l.Prepared(ev.CallID).Data["allocation"].(map[string]any)
				writeFile(t, filepath.Join(str(alloc, "path"), "a", "late.txt"), "written after the executor's turn\n")
			})
		}
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "unit_check_tree_moved" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	checkOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor"})
	if n := len(l.Checker.Calls("A")); n != 0 {
		t.Errorf("the tool was run %d times on a moved tree", n)
	}
	rows := l.FlowRows(engine.EvEscalation)
	if len(rows) != 1 || str(rows[0].Data, "ask") != "configuration" || str(rows[0].Data, "reason") != "unit_check_tree_moved" {
		t.Errorf("escalations %+v", rows)
	}
}

// A unit check whose tool failed, or whose worktree changed under it, escalates as a configuration question, and nothing is fixed.
func TestUnitCheckToolFailureEscalatesNoFix(t *testing.T) {
	for _, state := range []reviewadapter.State{reviewadapter.StateToolFailed, reviewadapter.StateWorktreeChanged} {
		t.Run(string(state), func(t *testing.T) {
			l := NewLab(t, oneNode()...)
			l.UnitChecks("A", UnitCheck{State: state})
			got := mustRunIncomplete(t, l)
			if st, reason := laneOf(t, got, "A"); st != "escalated" || reason != "unit_check_"+string(state) {
				t.Fatalf("lane A: %s %s", st, reason)
			}
			checkOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor"})
			rows := l.FlowRows(engine.EvEscalation)
			if len(rows) != 1 || str(rows[0].Data, "ask") != "configuration" {
				t.Errorf("escalations %+v", rows)
			}
		})
	}
}

// An invalid answer of the tool is asked once more with the same request (crw.unitCheck's retry); a second invalid answer escalates,
// and a verdict of the second answer is the check.
func TestUnitCheckInvalidRetriesOnceThenEscalates(t *testing.T) {
	t.Run("escalates", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.UnitChecks("A", UnitCheck{State: reviewadapter.StateInvalid}, UnitCheck{State: reviewadapter.StateInvalid})
		got := mustRunIncomplete(t, l)
		if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "unit_check_invalid" {
			t.Fatalf("lane A: %s %s", state, reason)
		}
		checkOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor"})
		calls := l.Checker.Calls("A")
		if len(calls) != 2 || calls[0].Request.Prompt != calls[1].Request.Prompt {
			t.Errorf("the checks %+v, want the same request twice", calls)
		}
	})
	t.Run("the retry answers", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.UnitChecks("A", UnitCheck{State: reviewadapter.StateInvalid})
		got := mustRun(t, l)
		if got.State != "closed" {
			t.Fatalf("state %q", got.State)
		}
		if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor", "reviewer-final:M1", "reflection:M1"}) {
			t.Fatalf("calls %v", labels)
		}
		if rows := l.Rows(ops.OpUnitCheck); len(rows) != 1 || rows[0].Data["retried"] != true || str(rows[0].Data, "first_state") != "invalid" {
			t.Errorf("unit-check rows %+v", rows)
		}
		noTierStep(t, l)
	})
}

// ---- the worktree of a null_model rebuild ----

// A fix turn that ends null_model leaves its edits in the attempt worktree that is retired; the rebuild starts in a new attempt
// worktree at the node's last commit with that commit's files, and the old worktree's uncommitted edits are only on the retired
// ref. There is no foreign verdict and no hold. The fix is the one of a failed verification of the first commit H1.
func TestNullModelRebuildStartsFromLastCommitInNewWorktree(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.Verify.Fail = map[string][]int{"A": {1}}
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
	if !slices.Equal(labels[:5], []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "rebuild:A:executor:r2"}) {
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

// The built-in script passes the static rules of `crw flow lint`: the lifecycle of every executor node (build, unit check, commit,
// verify, land), the tier step before every executor-t1, the reviewer's family, the labels, and the lanes' reads of outer
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

// TestUnitCheckHoldOfTheLaneHasSourceUnitCheck (CRW-1465): a unit check that gave no judgment for the tool's quota holds the node: the
// lane writes crw.hold with source unit_check and reason unit_check_quota, commits nothing, calls no reviewer and counts no tier
// evidence. A resume asks the checker again (the held check is no result to replay), and the node lands without a new executor call,
// as a verification that could not run is asked again on a resume (TestVerifierBackAfterResumeVerifiesAgain).
func TestUnitCheckHoldOfTheLaneHasSourceUnitCheck(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.UnitChecks("A", UnitCheck{State: reviewadapter.StateQuota})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "held" || reason != "unit_check_quota" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor"}) {
		t.Fatalf("calls %v", labels)
	}
	holds := l.Rows("crw.hold")
	if len(holds) != 1 || str(holds[0].Data, "reason") != "unit_check_quota" || str(holds[0].Data, "source") != "unit_check" {
		t.Fatalf("hold rows %+v", holds)
	}
	if n := len(l.Rows("crw.commit")); n != 0 {
		t.Errorf("%d commits under a held unit check", n)
	}
	if n := len(l.Checker.Calls("A")); n != 1 {
		t.Errorf("%d unit checks before the resume, want 1", n)
	}
	noTierStep(t, l)
	before := l.LastSeq()
	l.NewProcess("proc-2")
	final := Value[laneResult](t, l.Resume())
	if final.State != "closed" {
		t.Fatalf("resume: %+v; labels %v", final, l.Labels())
	}
	if n := len(l.Checker.Calls("A")); n != 2 {
		t.Errorf("%d unit checks, want the held one and the one the resume asked", n)
	}
	for _, c := range l.LiveCalls(before) {
		if strings.HasPrefix(c.Label, "build:") || strings.HasPrefix(c.Label, "fix:") || strings.HasPrefix(c.Label, "review:") {
			t.Errorf("the resume ran %q", c.Label)
		}
	}
	if !slices.Equal(l.Relay.Landed, []string{"A"}) {
		t.Errorf("landed %v", l.Relay.Landed)
	}
}

// TestUnitCheckFixPacketRendersTheFindingsWithoutAVerdict (CRW-1465, parent F5): the fix packet of a unit check is built from the check's
// verdict and findings, which carry no defects or criteria of a reviewer's VERDICT: it names every finding by its severity, id, file,
// line and text, says the executor may answer needs_research on the same thread, and renders no REVIEW or CRITERIA NOT MET section. A
// failed verification still builds its packet with the same signature.
func TestUnitCheckFixPacketRendersTheFindingsWithoutAVerdict(t *testing.T) {
	run := func(expr string) string {
		t.Helper()
		prog, err := loadScript("import { fixPacket } from 'crw:lib/packets'\nexport const meta = { name: 't' }\nreturn " + expr + "\n")
		if err != nil {
			t.Fatal(err)
		}
		res, err := engine.Run(context.Background(), prog, engine.Config{Backend: engine.NewFake(), Policy: engine.FakePolicy()})
		if err != nil || res.Status != engine.RunCompleted {
			t.Fatalf("fixPacket: %v %+v", err, res)
		}
		var text string
		if err := json.Unmarshal(res.Value, &text); err != nil {
			t.Fatalf("the packet %s: %v", res.Value, err)
		}
		return text
	}
	text := run(`fixPacket('A', { verdict: 'needs_changes', findings: [{ id: 'f_1', severity: 'P2', impact: 'core_function', file: 'a/x.go', line: 7, text: 'the loop never ends' }, ` +
		`{ id: 'f_2', severity: 'P3', impact: 'minor_separable', file: 'a/y.go', line: 2, text: 'a name' }] }, null, null, { summary: 'wrote the loop' })`)
	for _, want := range []string{"ROLE: executor. Node A: fix what the run found", "UNIT CHECK (needs_changes", "- [P2] f_1 a/x.go:7 the loop never ends", "- [P3] f_2 a/y.go:2 a name",
		"needs_research", "YOUR LAST RECEIPT: wrote the loop"} {
		if !strings.Contains(text, want) {
			t.Errorf("the unit-check fix packet lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "REVIEW (") || strings.Contains(text, "CRITERIA NOT MET") || strings.Contains(text, "FINDINGS OF THE EXPLORER") {
		t.Errorf("the unit-check fix packet renders a reviewer's verdict or notes:\n%s", text)
	}
	ci := run(`fixPacket('A', null, { pass: false, command: 'crw-dev ci local', failures: ['TestX failed'], summary: 'fail' }, null, { summary: 'wrote the loop' })`)
	if !strings.Contains(ci, "VERIFICATION FAILED (crw-dev ci local)") || !strings.Contains(ci, "- TestX failed") || strings.Contains(ci, "UNIT CHECK") {
		t.Errorf("the verification fix packet:\n%s", ci)
	}
}

// TestUnitCheckCarriedFindingSurvivesAVerificationFix (CRW-1465): a non-blocking finding carried past the last unit-check round trip stays
// carried to the land when the head then fails its verification: the fix of the verification is checked again (its own uncommitted
// change, which need not show the carried finding), and the lane result of the landed node still names the carried id.
func TestUnitCheckCarriedFindingSurvivesAVerificationFix(t *testing.T) {
	l := NewLab(t, oneNode()...)
	minor := Finding("P3", "a name could be clearer")
	l.UnitChecks("A", NeedsChanges(Finding("P2", "the first thing is wrong")), NeedsChanges(Finding("P2", "a second thing is wrong")),
		UnitCheck{Findings: []reviewadapter.Finding{minor}})
	l.Verify.Fail = map[string][]int{"A": {1}}
	res := l.Run()
	got := Value[struct {
		State string `json:"state"`
		Lanes []struct {
			Node    string   `json:"node"`
			State   string   `json:"state"`
			Carried []string `json:"carried"`
		} `json:"lanes"`
	}](t, res)
	if got.State != "closed" || len(got.Lanes) != 1 || got.Lanes[0].State != "landed" {
		t.Fatalf("%+v; calls %v", got, l.Labels())
	}
	if id := reviewadapter.FindingID(minor.File, minor.Text); !slices.Equal(got.Lanes[0].Carried, []string{id}) {
		t.Errorf("the landed lane carries %v, want [%s]", got.Lanes[0].Carried, id)
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "fix:A:executor:r3", "reviewer-final:M1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Errorf("calls %v, want %v", labels, want)
	}
	if n := len(l.Checker.Calls("A")); n != 4 {
		t.Errorf("%d unit checks, want the three of the round trips and the one of the verification fix", n)
	}
	noTierStep(t, l)
}

// packetBody is a turn's text without its first line (the crw-flow-op line, which names the call).
func packetBody(text string) string {
	_, body, _ := strings.Cut(text, "\n")
	return body
}

// TestRebuildPacketCarriesTheLastFindingsAndCI (CRW-1469): a fresh executor thread of a node has none of the turns of the thread
// before it, so its packet carries the node's last state: the findings of the last unit check, the last verification (its command
// and its failures) and the summary of the last receipt. The rebuild after a null_model, the rebuild after a refusal the orchestrator
// let go on and the executor-t1 build each carry them; a null_model of the first build carries none, and that rebuild's packet is the
// first build's.
func TestRebuildPacketCarriesTheLastFindingsAndCI(t *testing.T) {
	finding := Finding("P2", "the guard is missing")
	findingLine := "- [P2] " + reviewadapter.FindingID(finding.File, finding.Text) + " node.txt:1 the guard is missing"
	const ciLine = "LAST VERIFICATION FAILED (" + ops.CILocalCommand + "):\n- validate/tests: failed"
	const summary = "fixed the failing test"
	carries := func(t *testing.T, l *Lab, label string) {
		t.Helper()
		text := textOfCall(l, callNamed(t, l, label).ID)
		for _, want := range []string{"LAST FINDINGS (", findingLine, ciLine, "LAST RECEIPT (", "): " + summary} {
			if !strings.Contains(text, want) {
				t.Errorf("the packet of %s lacks %q:\n%s", label, want, text)
			}
		}
	}
	// the build is checked clean, its head fails verification, the fix of it (summary) is checked with a finding
	failThenFind := func(l *Lab) {
		l.UnitChecks("A", UnitCheck{}, NeedsChanges(finding))
		l.Verify.Fail = map[string][]int{"A": {1}}
		l.Sequence("^fix:A:executor:r1$", func(c *Call) fakehostPlan {
			c.Edit()
			return Say(ReceiptWith("ready_for_review", map[string]any{"summary": summary}))
		})
	}

	t.Run("null_model on the first build", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		l.Sequence("^build:A:executor$", func(*Call) fakehostPlan { return Garbage() })
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, l.Labels())
		}
		build := textOfCall(l, callNamed(t, l, "build:A:executor").ID)
		rebuild := textOfCall(l, callNamed(t, l, "rebuild:A:executor:r1").ID)
		if strings.Contains(rebuild, "LAST ") {
			t.Errorf("the rebuild after a first-build null_model carries a last state:\n%s", rebuild)
		}
		if packetBody(rebuild) != packetBody(build) || packetBody(build) == "" {
			t.Errorf("the rebuild's packet is not the first build's:\n%s\n----\n%s", build, rebuild)
		}
	})

	t.Run("null_model rebuild", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		failThenFind(l)
		l.Sequence("^fix:A:executor:r2$", func(*Call) fakehostPlan { return Garbage() })
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, l.Labels())
		}
		want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "rebuild:A:executor:r3", "reviewer-final:M1", "reflection:M1"}
		if labels := l.Labels(); !slices.Equal(labels, want) {
			t.Fatalf("calls %v, want %v", labels, want)
		}
		carries(t, l, "rebuild:A:executor:r3")
		noTierStep(t, l)
	})

	t.Run("refused rebuild", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		failThenFind(l)
		l.Sequence("^fix:A:executor:r2$", func(*Call) fakehostPlan {
			return fakehostPlan{StartError: map[string]any{"code": -32600, "message": "bad request", "data": map[string]any{"codexErrorInfo": "badRequest"}}}
		})
		got := mustRunIncomplete(t, l)
		if state, _ := laneOf(t, got, "A"); state != "waiting" {
			t.Fatalf("lane A %s; calls %v", state, l.Labels())
		}
		esc := l.FlowRows(engine.EvEscalation)
		if len(esc) != 1 {
			t.Fatalf("escalations %+v", esc)
		}
		l.apply(ops.EvDecisionApplied, map[string]any{"ask": str(esc[0].Data, "escalation_id"), "node": "A", "decision": map[string]any{"action": "retry"}})
		l.NewProcess("proc-applied")
		res := l.Resume()
		if v := Value[laneResult](t, res); v.State != "closed" {
			t.Fatalf("after the applied row: %+v; calls %v", v, l.Labels())
		}
		carries(t, l, "rebuild:A:executor:r3")
	})

	t.Run("executor-t1 build", func(t *testing.T) {
		l := NewLab(t, oneNode()...)
		// the fix of the failed head is checked with a finding, the fix of that with the same finding: SP-TIER-1 (a)
		failThenFind(l)
		l.UnitChecks("A", NeedsChanges(finding))
		if got := mustRun(t, l); got.State != "closed" {
			t.Fatalf("state %q; calls %v", got.State, l.Labels())
		}
		want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "build:A:executor-t1", "reviewer-final:M1", "reflection:M1"}
		if labels := l.Labels(); !slices.Equal(labels, want) {
			t.Fatalf("calls %v, want %v", labels, want)
		}
		// the last receipt is the fix r2's: the default receipt of the lab
		text := textOfCall(l, callNamed(t, l, "build:A:executor-t1").ID)
		for _, want := range []string{"LAST FINDINGS (", findingLine, ciLine, "): done: ready_for_review", "CHECKPOINT ("} {
			if !strings.Contains(text, want) {
				t.Errorf("the executor-t1 packet lacks %q:\n%s", want, text)
			}
		}
	})
}
