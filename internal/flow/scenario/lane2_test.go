package scenario

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/lint"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/script"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/workflows"
)

// A node the architect sized for a child goes up as a size split (CRW-1415): the `child` session path is retired, so the lane escalates it
// with reason size_split and detail child_retired, starts no child and calls no executor for it, closes its execution so no slot
// stays held, and the other nodes of the milestone are not held up by it.
func TestSizeFitChildEscalatesSizeSplitAndStartsNoChild(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "a big issue", Regions: []string{"a/"}, SizeFit: "child", SizeReason: "regions_gt3"},
		{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"b/"}},
	}
	l := NewLab(t, nodes...)
	// only a DESIGN recorded before S-1 sizes a node for a child (CRW-1435)
	l.UseLegacyDesign()
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

// A node the architect sized for a bundle split (CRW-1437) is too big for one node and goes up to the parent: the lane escalates it once
// with reason size_split, detail its size reason and ask bundle_split_request, calls no executor for it, writes no crw.decide for it (the
// node is closed: size_split closes its execution, so no slot stays held), and the other nodes of the milestone are not held up by it.
func TestBundleSplitEscalatesSizeSplitWithAsk(t *testing.T) {
	nodes := []NodeSpec{
		{ID: "A", Issue: "CRW-A", Title: "a bundle-sized issue", Regions: []string{"a/"}, SizeFit: "bundle_split", SizeReason: "regions_gt3"},
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
	if len(esc) != 1 || esc[0].Op != ops.OpEscalate || str(esc[0].Data, "reason") != "size_split" {
		t.Fatalf("escalations of A %+v, want one size_split", esc)
	}
	if d, a := str(esc[0].Data, "detail"), str(esc[0].Data, "ask"); d != "regions_gt3" || a != "bundle_split_request" {
		t.Errorf("the escalation of A has detail %q ask %q, want the size reason and bundle_split_request", d, a)
	}
	if ev, _ := esc[0].Data["evidence"].(map[string]any); str(ev, "size_reason") != "regions_gt3" {
		t.Errorf("the escalation of A carries evidence %v, want the size reason", esc[0].Data["evidence"])
	}
	for _, c := range l.Calls() {
		if c.Node == "A" {
			t.Errorf("a call for the bundle-split node: %+v", c)
		}
	}
	for _, r := range l.Rows(ops.OpDecide) {
		if str(r.Data, "node") == "A" {
			t.Errorf("a crw.decide row for the bundle-split node: %+v", r)
		}
	}
	if !slices.Contains(l.Relay.CloseReasons(), "A:size_split") {
		t.Errorf("closes %v: A's execution was not closed with size_split", l.Relay.CloseReasons())
	}
	if l.Relay.SlotHeld("A") {
		t.Errorf("a slot is held for the bundle-split node")
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
	// only a DESIGN recorded before S-1 sizes a node for a child (CRW-1435)
	l.UseLegacyDesign()
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

// An executor that commits its own work (here an App Server executor, whose backend records no executor_head_moved) leaves an
// uncommitted tree with nothing in it to judge (CRW-1463, DI 11c): the check reads the worktree's HEAD against the commit the attempt
// started at, does not run the tool, and the node escalates unit_check_head_moved as a configuration question; nothing is committed by
// the run and nothing lands.
func TestUnitCheckOfAnExecutorThatCommittedItsWorkEscalates(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.On("^build:A:executor$", func(c *Call) *fakehostPlan {
		c.Edit()
		l.GitIn(c.Cwd, "add", "-A")
		l.GitIn(c.Cwd, "-c", "commit.gpgsign=false", "commit", "-q", "-m", "the executor committed its own work")
		p := Say(Receipt("ready_for_review"))
		return &p
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "unit_check_head_moved" {
		t.Fatalf("lane A: %s %s; calls %v", state, reason, l.Labels())
	}
	checkOnly(t, l, []string{"architect", "design-audit:r1", "build:A:executor"})
	if n := len(l.Checker.Calls("A")); n != 0 {
		t.Errorf("the tool was run %d times on an executor's own commit", n)
	}
	if n := len(l.Rows("crw.commit")); n != 0 || len(l.Relay.Landed) != 0 {
		t.Errorf("%d commits, landed %v", n, l.Relay.Landed)
	}
	rows := l.FlowRows(engine.EvEscalation)
	if len(rows) != 1 || str(rows[0].Data, "ask") != "configuration" || str(rows[0].Data, "reason") != "unit_check_head_moved" {
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
		if labels := l.Labels(); !slices.Equal(labels, []string{"architect", "design-audit:r1", "build:A:executor", "reflection:M1"}) {
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
		if label := labelOf(l, ts.call); !strings.Contains(label, ":"+waiting+":") && label != "reflection:M1" {
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

// The research cap of a tier counts the reused answers too (CRW-1470), and only crw.exploreRoute counts it: an executor that asks
// the same question three times gets one explorer, the reuse of its answer as the second continuation, and the third request -
// itself a reuse - escalates once as a scope question with the detail the route names. The routes crw.exploreRoute recorded are
// spawn, reuse and escalate research_cap: the third route is the Go cap's own, not a reuse a counter of the script turned away.
func TestResearchCapCountsReuseAndEscalatesOnce(t *testing.T) {
	l := NewLab(t, oneNode()...)
	l.On("^(build|research):A:executor", func(c *Call) *fakehostPlan {
		p := Say(ReceiptWith("needs_research", map[string]any{"research_question": "which module owns the case?"}))
		return &p
	})
	got := mustRunIncomplete(t, l)
	if state, reason := laneOf(t, got, "A"); state != "escalated" || reason != "blocked_scope" {
		t.Fatalf("lane A: %s %s", state, reason)
	}
	if n := len(l.CallsOf("explorer")); n != 1 {
		t.Errorf("%d explorer calls, want one: the answer is reused", n)
	}
	if n := len(l.CallsOf("executor")); n != 3 {
		t.Errorf("%d executor calls, want the build and two research continuations", n)
	}
	if got, want := researchRoutes(t, l, "A"), []string{"spawn", "reuse answered_in_run", "escalate research_cap"}; !slices.Equal(got, want) {
		t.Errorf("the research routes of A %v, want %v", got, want)
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

// researchRoutes are the routes the completed crw.exploreRoute rows of the node's research questions recorded, in order, each as its
// route and, when it has one, its reason.
func researchRoutes(t *testing.T, l *Lab, node string) []string {
	t.Helper()
	var out []string
	for _, r := range l.Rows("crw.exploreRoute") {
		if str(r.Data, "kind") != "research" || str(r.Data, "node") != node {
			continue
		}
		raw, err := json.Marshal(r.Data["routes"])
		if err != nil {
			t.Fatal(err)
		}
		var routes []struct{ Route, Reason string }
		if err := json.Unmarshal(raw, &routes); err != nil {
			t.Fatalf("the routes of %v: %v", r.Data, err)
		}
		for _, rt := range routes {
			out = append(out, strings.TrimSpace(rt.Route+" "+rt.Reason))
		}
	}
	return out
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

// A node whose head is already on the integration tip lands as a replay and frees its slot and its regions as a real land does
// (CRW-1461): the node B, whose regions overlap the regions of the landed node A, is released and not held back defer:edit_overlap
// for a node that has landed.
func TestIntegrateOfHeadOnTipFreesTheNode(t *testing.T) {
	l := NewLab(t,
		NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a.go"}},
		NodeSpec{ID: "B", Issue: "CRW-B", Title: "issue B", Regions: []string{"a.go"}},
	)
	r := l.Relay
	r.Slots = 2
	r.HoldBackIf = r.relayRegionRule
	ctx := context.Background()
	relA, err := r.Release(ctx, ops.ReleaseRequest{Node: "A", Regions: []string{"a.go"}})
	if err != nil || !relA.Released {
		t.Fatalf("release of A: %+v, %v", relA, err)
	}
	// the head is the base, which the integration branch already holds
	acc := ops.AcceptRequest{Node: "A", Head: l.Base, Base: l.Base}
	r.mu.Lock()
	r.accepted["acc-A"] = acc
	r.mu.Unlock()
	got, err := r.Integrate(ctx, ops.IntegrateRequest{Node: "A", Head: l.Base, AcceptanceID: "acc-A"})
	if err != nil || !got.Landed || !got.Replayed {
		t.Fatalf("integrate of A: %+v, %v; want landed and replayed", got, err)
	}
	if r.SlotHeld("A") {
		t.Errorf("A holds its slot after it landed")
	}
	relB, err := r.Release(ctx, ops.ReleaseRequest{Node: "B", Regions: []string{"a.go"}})
	if err != nil {
		t.Fatalf("release of B: %v", err)
	}
	if !relB.Released {
		t.Errorf("B was not released: reason %q, want released after A landed", relB.Reason)
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
	want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "fix:A:executor:r3", "reflection:M1"}
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
		want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "rebuild:A:executor:r3", "reflection:M1"}
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
		want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "fix:A:executor:r2", "build:A:executor-t1", "reflection:M1"}
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

// ---- the sub-tasks of a split node (CRW-1459) ----

// splitLab is a lab of one node A sized for a split whose sub-tasks are ids, in that order, each with its own file a/<id>.txt.
func splitLab(t *testing.T, ids ...string) *Lab {
	t.Helper()
	return splitLabWith(t, nil, ids...)
}

// splitLabWith is splitLab with the node's criteria (none: the lab's default criterion).
func splitLabWith(t *testing.T, criteria []string, ids ...string) *Lab {
	t.Helper()
	l := NewLab(t, NodeSpec{ID: "A", Issue: "CRW-A", Title: "issue A", Regions: []string{"a/"}, SizeFit: "split", SizeReason: "regions_gt3", Criteria: criteria})
	d := l.designFor()
	for _, id := range ids {
		d.Issues[0].SubTasks = append(d.Issues[0].SubTasks, ops.SubTask{ID: id, Summary: "write " + id, EditRegions: []string{"a/" + id + ".txt"}, DoneCondition: "a/" + id + ".txt is written"})
	}
	l.Design = &d
	return l
}

// writes answers an executor turn of node A with a receipt of outcome after it wrote a/<id>.txt.
func writes(id, outcome string) func(*Call) fakehostPlan {
	return func(c *Call) fakehostPlan {
		c.EditPath("a/"+id+".txt", id+"\n")
		return Say(ReceiptWith(outcome, map[string]any{"summary": "wrote " + id, "gap": map[bool]any{true: "the " + id + " part is missing"}[outcome == "failed"]}))
	}
}

// rowsOfNode are the completed rows of op whose node is node.
func rowsOfNode(l *Lab, op, node string) []engine.Event {
	var out []engine.Event
	for _, r := range l.Rows(op) {
		if str(r.Data, "node") == node {
			out = append(out, r)
		}
	}
	return out
}

// noJudgementBefore asserts that no crw.verify, crw.review, crw.accept or crw.integrate row of node A comes before seq.
func noJudgementBefore(t *testing.T, l *Lab, seq int, what string) {
	t.Helper()
	for _, op := range []string{ops.OpVerify, "crw.review", ops.OpAccept, ops.OpIntegrate} {
		for _, r := range rowsOfNode(l, op, "A") {
			if r.Seq < seq {
				t.Errorf("a %s row (seq %d) before %s (seq %d)", op, r.Seq, what, seq)
			}
		}
	}
}

// TestSplitNodeRunsSubTasksInOrderOnOneExecutorThread (CRW-1459, S-3): a node sized for a split with sub-tasks runs them in their
// order on one executor thread: the build does the first, and each next one is a turn of the same thread (subtask:A:executor:s2,
// :s3). Each sub-task before the last ends in a checkpoint crw.commit of the node; the node is verified and landed once, after the
// receipt of the last sub-task, and its last commit holds every sub-task's file.
func TestSplitNodeRunsSubTasksInOrderOnOneExecutorThread(t *testing.T) {
	l := splitLab(t, "one", "two", "three")
	var mu sync.Mutex
	threads := map[string]string{}
	record := func(id string) func(*Call) fakehostPlan {
		w := writes(id, "ready_for_review")
		return func(c *Call) fakehostPlan {
			mu.Lock()
			threads[c.Label] = c.ThreadID
			mu.Unlock()
			return w(c)
		}
	}
	l.Sequence("^build:A:executor$", record("one"))
	l.Sequence("^subtask:A:executor:s2$", record("two"))
	l.Sequence("^subtask:A:executor:s3$", record("three"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "subtask:A:executor:s3", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	build, s2, s3 := callNamed(t, l, "build:A:executor"), callNamed(t, l, "subtask:A:executor:s2"), callNamed(t, l, "subtask:A:executor:s3")
	if s2.Resume != build.ID || s3.Resume != s2.ID {
		t.Errorf("the sub-task calls resume %q and %q, want %q and %q", s2.Resume, s3.Resume, build.ID, s2.ID)
	}
	if th := threads["build:A:executor"]; th == "" || threads["subtask:A:executor:s2"] != th || threads["subtask:A:executor:s3"] != th {
		t.Errorf("threads %v, want one thread", threads)
	}
	commits := rowsOfNode(l, ops.OpCommit, "A")
	if len(commits) != 3 {
		t.Fatalf("%d crw.commit rows of A, want 3 (two checkpoints and the node's)", len(commits))
	}
	if !(commits[0].Seq < s2.Seq && s2.Seq < commits[1].Seq && commits[1].Seq < s3.Seq && s3.Seq < commits[2].Seq) {
		t.Errorf("commits at %d, %d, %d; sub-task calls at %d, %d", commits[0].Seq, commits[1].Seq, commits[2].Seq, s2.Seq, s3.Seq)
	}
	if str(commits[1].Data, "parent") != str(commits[0].Data, "commit") || str(commits[2].Data, "parent") != str(commits[1].Data, "commit") {
		t.Errorf("the commits of A do not chain: %v", commits)
	}
	noJudgementBefore(t, l, s3.Seq, "the last sub-task's call")
	for _, op := range []string{ops.OpVerify, ops.OpAccept, ops.OpIntegrate} {
		if n := len(rowsOfNode(l, op, "A")); n != 1 {
			t.Errorf("%d %s rows of A, want 1", n, op)
		}
	}
	last := str(commits[2].Data, "commit")
	for _, id := range []string{"one", "two", "three"} {
		if out := l.Git("show", last+":a/"+id+".txt"); strings.TrimSpace(out) != id {
			t.Errorf("the last commit of A has a/%s.txt %q", id, out)
		}
	}
	first := textOfCall(l, build.ID)
	for _, w := range []string{"SUB-TASKS (", "- 1. one (to do): write one", "- 3. three (to do): write three", "Do sub-task 1 (one) now, and only it"} {
		if !strings.Contains(first, w) {
			t.Errorf("the build packet lacks %q:\n%s", w, first)
		}
	}
	second := textOfCall(l, s2.ID)
	for _, w := range []string{"sub-task 2 of 3 (two): write two", "a/two.txt", "a/two.txt is written"} {
		if !strings.Contains(second, w) {
			t.Errorf("the s2 packet lacks %q:\n%s", w, second)
		}
	}
	noTierStep(t, l)
}

// TestSplitSubTaskSelfFailedGoesOnWithoutAJudgement (CRW-1459, S-3, R1-01): a sub-task before the last that ends self_failed is no
// judgement of the node: its tree gets the checkpoint commit and the same thread goes on to the next sub-task, whose packet names it
// as reported failed, with its summary and gap; no verification, review or land of the node comes before that call, and the node lands
// after the last sub-task. The failed receipt is no tier evidence.
func TestSplitSubTaskSelfFailedGoesOnWithoutAJudgement(t *testing.T) {
	l := splitLab(t, "one", "two")
	l.Sequence("^build:A:executor$", writes("one", "failed"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	build, s2 := callNamed(t, l, "build:A:executor"), callNamed(t, l, "subtask:A:executor:s2")
	if s2.Resume != build.ID {
		t.Errorf("the s2 call resumes %q, want the build %q", s2.Resume, build.ID)
	}
	commits := rowsOfNode(l, ops.OpCommit, "A")
	if len(commits) != 2 || !(build.Seq < commits[0].Seq && commits[0].Seq < s2.Seq) || str(commits[0].Data, "call") != build.ID {
		t.Fatalf("commits of A %v, want the checkpoint of the build's call before the s2 call (seq %d)", commits, s2.Seq)
	}
	noJudgementBefore(t, l, s2.Seq, "the s2 call")
	text := textOfCall(l, s2.ID)
	for _, w := range []string{"reported failed", "wrote one", "the one part is missing", "it is part of this node and is judged with it"} {
		if !strings.Contains(text, w) {
			t.Errorf("the s2 packet lacks %q:\n%s", w, text)
		}
	}
	if n := len(rowsOfNode(l, ops.OpIntegrate, "A")); n != 1 {
		t.Errorf("%d crw.integrate rows of A, want 1", n)
	}
	last := str(commits[1].Data, "commit")
	for _, id := range []string{"one", "two"} {
		if out := l.Git("show", last+":a/"+id+".txt"); strings.TrimSpace(out) != id {
			t.Errorf("the last commit of A has a/%s.txt %q", id, out)
		}
	}
	noTierStep(t, l)
}

// TestSplitNodeKeepsEarlierSubTasksAcrossARebuildAndATierStep (CRW-1459, S-5, R1-02): the checkpoint commits carry the earlier
// sub-tasks into a fresh thread. The first sub-task writes a/one.txt and is checkpointed; the second ends null_model, its rebuild ends
// null_model again (SP-TIER-1 c) and the tier steps. The rebuild and the executor-t1 build each start at sub-task 2 (their packets say
// so) in a worktree at the checkpoint, which holds a/one.txt; the node's last commit holds both files, and the step row's checkpoint
// head is the first sub-task's commit.
func TestSplitNodeKeepsEarlierSubTasksAcrossARebuildAndATierStep(t *testing.T) {
	l := splitLab(t, "one", "two")
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", func(*Call) fakehostPlan { return Garbage() })
	l.Sequence("^rebuild:A:executor:", func(*Call) fakehostPlan { return Garbage() })
	var atStart struct{ head, file string }
	l.Sequence("^build:A:executor-t1$", func(c *Call) fakehostPlan {
		atStart.head = l.GitIn(c.Cwd, "rev-parse", "HEAD")
		b, _ := os.ReadFile(filepath.Join(c.Cwd, "a", "one.txt"))
		atStart.file = string(b)
		return writes("two", "ready_for_review")(c)
	})
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "rebuild:A:executor:r2", "build:A:executor-t1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	commits := rowsOfNode(l, ops.OpCommit, "A")
	if len(commits) != 2 {
		t.Fatalf("%d crw.commit rows of A, want the checkpoint and the node's", len(commits))
	}
	c1 := str(commits[0].Data, "commit")
	if atStart.head != c1 || strings.TrimSpace(atStart.file) != "one" {
		t.Errorf("the executor-t1 build started at %s with a/one.txt %q, want the checkpoint %s holding it", atStart.head, atStart.file, c1)
	}
	steps := l.StepRows()
	if len(steps) != 1 || steps[0].Head != c1 {
		t.Errorf("step rows %+v, want one with checkpoint head %s", steps, c1)
	}
	for _, label := range []string{"rebuild:A:executor:r2", "build:A:executor-t1"} {
		text := textOfCall(l, callNamed(t, l, label).ID)
		for _, w := range []string{"- 1. one (done): write one", "- 2. two (to do): write two", "Do sub-task 2 (two) now, and only it"} {
			if !strings.Contains(text, w) {
				t.Errorf("the packet of %s lacks %q:\n%s", label, w, text)
			}
		}
	}
	last := str(commits[1].Data, "commit")
	if str(commits[1].Data, "parent") != c1 {
		t.Errorf("the node's commit is on %s, want the checkpoint %s", commits[1].Data["parent"], c1)
	}
	for _, id := range []string{"one", "two"} {
		if out := l.Git("show", last+":a/"+id+".txt"); strings.TrimSpace(out) != id {
			t.Errorf("the last commit of A has a/%s.txt %q", id, out)
		}
	}
	noJudgementBefore(t, l, callNamed(t, l, "build:A:executor-t1").Seq, "the executor-t1 build")
}

// splitCriteria are criteria of a split node A with the sub-tasks one and two, each met by one sub-task's file.
var splitCriteria = []string{"a/one.txt holds one", "a/two.txt holds two"}

var criterionLine = regexp.MustCompile(`(?m)^c\d+: (a/\S+) holds (\S+)$`)

// criteriaChecker is a checker that does what the prompt asks: every criterion the prompt names (a `cN: a/<file> holds <word>` line)
// is judged in the worktree, and one the tree does not meet is a finding; a prompt that names no criterion is verified.
func criteriaChecker(c CheckInfo) (UnitCheck, bool) {
	var found []reviewadapter.Finding
	for _, m := range criterionLine.FindAllStringSubmatch(c.Prompt, -1) {
		b, _ := os.ReadFile(filepath.Join(c.Worktree, m[1]))
		if strings.TrimSpace(string(b)) != m[2] {
			found = append(found, reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: m[1], Line: 1, Text: "the criterion " + m[1] + " holds " + m[2] + " is not met"})
		}
	}
	if len(found) > 0 {
		return NeedsChanges(found...), true
	}
	return UnitCheck{}, true
}

// TestASplitSubTaskIsCheckedAgainstItsOwnDoneCondition (CRW-1459, the crosscheck's tiers_exhausted sequence): the unit check of a
// sub-task judges that sub-task, against its own done condition and edit regions, and says the later sub-tasks are not due yet; it
// names none of the node's criteria. With a checker that reports every criterion its prompt names and the tree does not meet, a
// correct first sub-task is not failed for the second sub-task's criterion: the second sub-task runs on the same thread and the node
// lands with no tier step (a check of the whole node's criteria on the first sub-task ran two fix rounds, stepped the tier and ended
// tiers_exhausted after four checks, with no commit and no second sub-task).
func TestASplitSubTaskIsCheckedAgainstItsOwnDoneCondition(t *testing.T) {
	l := splitLabWith(t, splitCriteria, "one", "two")
	l.OnUnitCheck(criteriaChecker)
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	noTierStep(t, l)
	for _, ev := range l.FlowRows(engine.EvEscalation) {
		t.Errorf("an escalation %v", ev.Data)
	}
	checks := l.Checker.Calls("A")
	if len(checks) < 2 {
		t.Fatalf("%d unit checks of A, want the two sub-tasks' and the whole node's", len(checks))
	}
	first, second := checks[0].Request.Prompt, checks[1].Request.Prompt
	for _, w := range []string{"Review the uncommitted changes in this working tree.", "unit check of node A (issue CRW-A) of a crw run, of its sub-task 1 of 2 (one) only",
		"SUB-TASK 1 of 2 (one): write one", "DONE CONDITION: a/one.txt is written", "EDIT REGIONS: a/one.txt", "NOT DUE YET", "- 2. two: write two"} {
		if !strings.Contains(first, w) {
			t.Errorf("the first sub-task's check lacks %q:\n%s", w, first)
		}
	}
	for _, w := range []string{"sub-task 2 of 2 (two) only", "DONE CONDITION: a/two.txt is written", "EARLIER SUB-TASKS", "- 1. one: write one"} {
		if !strings.Contains(second, w) {
			t.Errorf("the last sub-task's check lacks %q:\n%s", w, second)
		}
	}
	for i, p := range []string{first, second} {
		if criterionLine.MatchString(p) || strings.Contains(p, "CRITERIA") {
			t.Errorf("the check of sub-task %d names the node's criteria:\n%s", i+1, p)
		}
	}
	rows := rowsOfNode(l, ops.OpUnitCheck, "A")
	if len(rows) < 2 || str(rows[0].Data, "sub") != "one" || str(rows[1].Data, "sub") != "two" {
		t.Errorf("the unit-check rows of A name the sub-tasks %v", rows)
	}
}

// TestASplitNodeIsAcceptedOnTheCheckOfItsWholeChange (CRW-1459): after the last sub-task's commit and its verification, the lane checks
// the node's whole change, the committed range from the node's base to its head, against every criterion of the node, and the
// acceptance rests on that check alone. A finding of it goes back to the same executor thread as a fix round; the fix is checked,
// committed and verified, and the whole change is checked again; the node lands on that second check of the whole change.
func TestASplitNodeIsAcceptedOnTheCheckOfItsWholeChange(t *testing.T) {
	l := splitLabWith(t, splitCriteria, "one", "two")
	whole := 0
	l.OnUnitCheck(func(c CheckInfo) (UnitCheck, bool) {
		if !strings.HasPrefix(c.Prompt, "Review the changes between ") {
			return UnitCheck{}, false
		}
		whole++
		if whole == 1 {
			return NeedsChanges(reviewadapter.Finding{Severity: "P2", Impact: "core_function", File: "a/two.txt", Line: 1, Text: "the parts do not agree"}), true
		}
		return UnitCheck{}, false
	})
	l.OnUnitCheck(criteriaChecker)
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	l.Sequence("^fix:A:executor:", writes("agree", "ready_for_review"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "fix:A:executor:r2", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	s2, fix := callNamed(t, l, "subtask:A:executor:s2"), callNamed(t, l, "fix:A:executor:r2")
	if fix.Resume != s2.ID {
		t.Errorf("the fix resumes %q, want the last sub-task's call %q", fix.Resume, s2.ID)
	}
	noTierStep(t, l)
	commits := rowsOfNode(l, ops.OpCommit, "A")
	if len(commits) != 3 {
		t.Fatalf("%d crw.commit rows of A, want the checkpoint, the node's and the fix's", len(commits))
	}
	base, head1, head2 := str(commits[0].Data, "parent"), str(commits[1].Data, "commit"), str(commits[2].Data, "commit")
	var wholeRows []engine.Event
	for _, r := range rowsOfNode(l, ops.OpUnitCheck, "A") {
		if str(r.Data, "mode") == ops.UnitCheckCommitted {
			wholeRows = append(wholeRows, r)
		}
	}
	if len(wholeRows) != 2 {
		t.Fatalf("%d checks of the whole change of A, want 2", len(wholeRows))
	}
	for i, h := range []string{head1, head2} {
		if r := wholeRows[i]; str(r.Data, "commit") != h || str(r.Data, "base") != base || str(r.Data, "sub") != "" {
			t.Errorf("check %d of the whole change: %v, want the range %s..%s", i+1, r.Data, base, h)
		}
	}
	for _, r := range rowsOfNode(l, ops.OpUnitCheck, "A") {
		if r.Seq > fix.Seq && str(r.Data, "sub") != "" {
			t.Errorf("the check of the fix after the last sub-task names the sub-task %v; it judges the node", r.Data)
		}
	}
	verifies := rowsOfNode(l, ops.OpVerify, "A")
	if len(verifies) != 2 || !(verifies[0].Seq < wholeRows[0].Seq && wholeRows[0].Seq < fix.Seq && verifies[1].Seq < wholeRows[1].Seq) {
		t.Errorf("verifies %v, checks of the whole change at %d and %d, the fix at %d", verifies, wholeRows[0].Seq, wholeRows[1].Seq, fix.Seq)
	}
	var prompt string
	for _, c := range l.Checker.Calls("A") {
		if strings.Contains(c.Request.Prompt, ".."+head2) {
			prompt = c.Request.Prompt
		}
	}
	for _, w := range []string{"git diff " + base + ".." + head2, "c1: a/one.txt holds one", "c2: a/two.txt holds two", "the whole change of the node"} {
		if !strings.Contains(prompt, w) {
			t.Errorf("the last check of the whole change lacks %q:\n%s", w, prompt)
		}
	}
	accepts := rowsOfNode(l, ops.OpAccept, "A")
	if len(accepts) != 1 || str(accepts[0].Data, "reviewer_call") != wholeRows[1].CallID || str(accepts[0].Data, "head") != head2 {
		t.Errorf("the acceptance %v, want it on the check %s of the whole change at %s", accepts, wholeRows[1].CallID, head2)
	}
}

// TestASplitNodeAfterATierStepPastItsLastSubTaskIsBuiltAsAWhole (CRW-1459, S-5): the node's commit after the last sub-task marks
// every sub-task done. The verification of that commit fails, its fix's commit fails again (SP-TIER-1 b) and the tier steps: the
// executor-t1 build starts at the node's last commit, which holds both sub-tasks, and its packet is the node's packet over it with
// every sub-task done (no sub-task to do now); its check judges the node, not a sub-task.
func TestASplitNodeAfterATierStepPastItsLastSubTaskIsBuiltAsAWhole(t *testing.T) {
	l := splitLab(t, "one", "two")
	l.Verify.Fail = map[string][]int{"A": {1, 2}}
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	l.Sequence("^fix:A:executor:", writes("fix", "ready_for_review"))
	l.Sequence("^build:A:executor-t1$", writes("t1", "ready_for_review"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "fix:A:executor:r2", "build:A:executor-t1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	t1 := callNamed(t, l, "build:A:executor-t1")
	text := textOfCall(l, t1.ID)
	for _, w := range []string{"- 1. one (done): write one", "- 2. two (done): write two", "Every sub-task is done and committed in your worktree"} {
		if !strings.Contains(text, w) {
			t.Errorf("the executor-t1 packet lacks %q:\n%s", w, text)
		}
	}
	if strings.Contains(text, "Do sub-task") || strings.Contains(text, "(to do)") {
		t.Errorf("the executor-t1 packet asks for a sub-task the node's commit holds:\n%s", text)
	}
	for _, r := range rowsOfNode(l, ops.OpUnitCheck, "A") {
		if r.Seq > t1.Seq && str(r.Data, "sub") != "" {
			t.Errorf("a check after the tier step names the sub-task %v", r.Data)
		}
	}
}

// TestARepeatedFindingOfTheWholeChangeStepsTheTier (CRW-1459, SP-TIER-1 a): two checks of a split node's whole change in a row that rule
// needs_changes on one finding are the evidence of a tier step, whatever the check of the fix between them found: that check judges the
// fix alone and is no reviewed round of the node when it verifies it. The executor-t1 build starts at the node's last commit and the node
// lands on the next check of its whole change.
func TestARepeatedFindingOfTheWholeChangeStepsTheTier(t *testing.T) {
	l := splitLab(t, "one", "two")
	whole := 0
	l.OnUnitCheck(func(c CheckInfo) (UnitCheck, bool) {
		if !strings.HasPrefix(c.Prompt, "Review the changes between ") {
			return UnitCheck{}, false
		}
		whole++
		if whole <= 2 {
			return NeedsChanges(reviewadapter.Finding{Severity: "P2", Impact: "core_function", File: "a/two.txt", Line: 1, Text: "the parts do not agree"}), true
		}
		return UnitCheck{}, false
	})
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	l.Sequence("^fix:A:executor:", writes("agree", "ready_for_review"))
	l.Sequence("^build:A:executor-t1$", writes("t1", "ready_for_review"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "build:A:executor", "subtask:A:executor:s2", "fix:A:executor:r2", "build:A:executor-t1", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	steps := l.StepRows()
	if len(steps) != 1 {
		t.Fatalf("step rows %+v, want one", steps)
	}
	if whole != 3 {
		t.Errorf("%d checks of the whole change, want 3", whole)
	}
}

// TestABrokenSubTaskRuleGoesBackToTheArchitect (CRW-1435, the DI's P2): an architect answer that breaks a sub-task rule the schema cannot
// express (a sub-task region outside the node's) passes the engine's schema check; the design audit makes it a finding and the DESIGN
// goes back to the architect for a design-fix round, whose DESIGN is approved, and the node lands. The run does not end script_error.
func TestABrokenSubTaskRuleGoesBackToTheArchitect(t *testing.T) {
	l := splitLab(t, "one", "two")
	bad := l.designFor()
	bad.Issues = slices.Clone(bad.Issues)
	bad.Issues[0].SubTasks = []ops.SubTask{bad.Issues[0].SubTasks[0], {ID: "two", Summary: "write two", EditRegions: []string{"b/two.txt"}, DoneCondition: "b/two.txt is written"}}
	l.Sequence("^architect$", func(*Call) fakehostPlan { return Say(bad) })
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	res := l.Run()
	if res.Status != engine.RunCompleted {
		t.Fatalf("run %s: %s: %s; calls %v", res.Status, res.ErrorKind, res.Error, l.Labels())
	}
	got := Value[laneResult](t, res)
	if got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	want := []string{"architect", "design-audit:r1", "design-fix:r1", "design-audit:r2", "build:A:executor", "subtask:A:executor:s2", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	audits := l.Rows(ops.OpDesignAudit)
	if len(audits) != 2 || str(audits[0].Data, "state") != ops.AuditNeedsChanges || str(audits[1].Data, "state") != ops.AuditApproved {
		t.Errorf("the audits %v", audits)
	}
	if fix := textOfCall(l, callNamed(t, l, "design-fix:r1").ID); !strings.Contains(fix, "b/two.txt") {
		t.Errorf("the design-fix packet does not name the broken rule:\n%s", fix)
	}
}
