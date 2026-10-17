package scenario

// CRW-1564: a split node's sub-tasks each get a scoped unit check of their own uncommitted change before their checkpoint
// commit — the first while the next sub-task's files do not exist — and the node's whole committed change still gets the one
// committed check the acceptance rests on. A scoped check that finds or answers nothing stops the checkpoint and the next
// sub-task.

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/flow/engine"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/ops"
	"github.com/thisisjun786/codex-relay-workflow/internal/flow/reviewadapter"
)

// doneCondition is the `DONE CONDITION: <file> is written` a scoped prompt carries for its sub-task.
var doneCondition = regexp.MustCompile(`DONE CONDITION: (a/\S+\.txt) is written`)

// scopedSubChecker is the lab's checker for the split tests: a scoped prompt (it carries SUB-TASK) is judged against the
// sub-task's done condition — its file must be in the worktree — and a whole-node prompt (the committed check) against every
// criterion line, as criteriaChecker. It records the scoped prompts in order and whether a/two.txt existed when the first
// sub-task was judged.
type scopedSubChecker struct {
	prompts       []string
	twoAtFirstSub bool
}

func (s *scopedSubChecker) check(c CheckInfo) (UnitCheck, bool) {
	if !strings.Contains(c.Prompt, "SUB-TASK") {
		return criteriaChecker(c)
	}
	s.prompts = append(s.prompts, c.Prompt)
	// a whole-node criterion inside a scoped prompt is still enforced: a scoped check that silently carried one could hide
	// the regression this checker is here to catch
	if got, _ := criteriaChecker(c); len(got.Findings) > 0 {
		return got, true
	}
	if strings.Contains(c.Prompt, "SUB-TASK 1 of ") {
		if _, err := os.Stat(filepath.Join(c.Worktree, "a/two.txt")); err == nil {
			s.twoAtFirstSub = true
		}
	}
	if m := doneCondition.FindStringSubmatch(c.Prompt); m != nil {
		if _, err := os.Stat(filepath.Join(c.Worktree, m[1])); err != nil {
			return NeedsChanges(reviewadapter.Finding{Severity: "P1", Impact: "core_function", File: m[1], Line: 1,
				Text: "the sub-task's done condition is not met: " + m[1] + " is missing"}), true
		}
	}
	return UnitCheck{}, true
}

// TestSubTasksAreCheckedScopedBeforeEachCommit: a split node's run checks the uncommitted change of every sub-task —
// including the last — before its checkpoint commit, then checks the node's whole committed range once: three checks total,
// the two scoped ones before their commits, the committed one after the last, and the acceptance's reviewer call is the
// committed check. The first scoped check runs while the second sub-task's file does not exist.
func TestSubTasksAreCheckedScopedBeforeEachCommit(t *testing.T) {
	sc := &scopedSubChecker{}
	l := splitLabWith(t, splitCriteria, "one", "two")
	l.OnUnitCheck(sc.check)
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
	commits := rowsOfNode(l, ops.OpCommit, "A")
	checks := l.Checker.Calls("A")
	rows := rowsOfNode(l, ops.OpUnitCheck, "A")
	if len(commits) != 2 || len(checks) != 3 || len(rows) != 3 || len(sc.prompts) != 2 {
		t.Fatalf("commits %d, checks %d, rows %d, scoped prompts %d", len(commits), len(checks), len(rows), len(sc.prompts))
	}
	// the scoped checks come before their sub-task's commit; the committed check after the node's last commit
	if rows[0].Seq > commits[0].Seq || rows[1].Seq > commits[1].Seq || rows[2].Seq < commits[1].Seq {
		t.Errorf("check rows %d,%d,%d against commits %d,%d: the scoped checks are not before their commits", rows[0].Seq, rows[1].Seq, rows[2].Seq, commits[0].Seq, commits[1].Seq)
	}
	if str(rows[0].Data, "mode") != ops.UnitCheckUncommitted || str(rows[1].Data, "mode") != ops.UnitCheckUncommitted || str(rows[2].Data, "mode") != ops.UnitCheckCommitted {
		t.Errorf("check modes: %v", []string{str(rows[0].Data, "mode"), str(rows[1].Data, "mode"), str(rows[2].Data, "mode")})
	}
	for _, w := range []string{"SUB-TASK 1 of 2 (one)", "DONE CONDITION: a/one.txt", "NOT DUE YET"} {
		if !strings.Contains(sc.prompts[0], w) {
			t.Errorf("the first scoped check lacks %q:\n%s", w, sc.prompts[0])
		}
	}
	for _, w := range []string{"SUB-TASK 2 of 2 (two)", "DONE CONDITION: a/two.txt", "EARLIER SUB-TASKS"} {
		if !strings.Contains(sc.prompts[1], w) {
			t.Errorf("the second scoped check lacks %q:\n%s", w, sc.prompts[1])
		}
	}
	// the first sub-task was judged while the second's file did not exist yet
	if sc.twoAtFirstSub {
		t.Error("the first sub-task's scoped check saw a/two.txt: it did not run before the second sub-task")
	}
	// the acceptance rests on the committed whole-node check, the last one
	accepts := rowsOfNode(l, ops.OpAccept, "A")
	if len(accepts) != 1 || str(accepts[0].Data, "reviewer_call") != rows[2].CallID {
		t.Errorf("the acceptance's reviewer call is not the committed check: %v", accepts)
	}
}

// TestScopedCheckFixThenSecondSubTask: a scoped check that finds the first sub-task's file missing sends a fix round on the
// same thread; the fixed change is checked again and only then is the sub-task committed and the second started.
func TestScopedCheckFixThenSecondSubTask(t *testing.T) {
	l := splitLabWith(t, splitCriteria, "one", "two")
	// the checker judges the done condition of the sub the prompt names
	sc := &scopedSubChecker{}
	l.OnUnitCheck(sc.check)
	// the first sub-task's executor reports ready without writing its file: the scoped check finds it and fixes it
	l.Sequence("^build:A:executor$", func(c *Call) fakehostPlan {
		return Say(ReceiptWith("ready_for_review", map[string]any{"summary": "wrote one"}))
	})
	l.Sequence("^fix:A:executor", writes("one", "ready_for_review"))
	l.Sequence("^subtask:A:executor:s2$", writes("two", "ready_for_review"))
	if got := mustRun(t, l); got.State != "closed" {
		t.Fatalf("state %q; calls %v", got.State, l.Labels())
	}
	// the scoped finding's fix round is the first sub-task's own thread; the second sub-task resumes that same thread after
	// the retry passed and the checkpoint commit landed
	want := []string{"architect", "design-audit:r1", "build:A:executor", "fix:A:executor:r1", "subtask:A:executor:s2", "reflection:M1"}
	if labels := l.Labels(); !slices.Equal(labels, want) {
		t.Fatalf("calls %v, want %v", labels, want)
	}
	build, fix, s2 := callNamed(t, l, "build:A:executor"), callNamed(t, l, "fix:A:executor:r1"), callNamed(t, l, "subtask:A:executor:s2")
	if fix.Resume != build.ID || s2.Resume != fix.ID {
		t.Errorf("the fix resumes %q and the s2 call %q, want the build %q and the fix %q: not one thread", fix.Resume, s2.Resume, build.ID, fix.ID)
	}
	commits := rowsOfNode(l, ops.OpCommit, "A")
	rows := rowsOfNode(l, ops.OpUnitCheck, "A")
	if len(commits) != 2 || len(rows) != 4 || len(sc.prompts) != 3 {
		t.Fatalf("commits %d, check rows %d, scoped prompts %d, want 2, 4 and 3", len(commits), len(rows), len(sc.prompts))
	}
	// the failed scoped check, the fix's verified retry, the second sub-task's scoped check, then the committed whole change
	if str(rows[0].Data, "verdict") != reviewadapter.VerdictNeedsChanges || str(rows[0].Data, "mode") != ops.UnitCheckUncommitted {
		t.Errorf("the first scoped check: %v", rows[0].Data)
	}
	for i, want := range map[int]string{1: reviewadapter.VerdictVerified, 2: reviewadapter.VerdictVerified} {
		if str(rows[i].Data, "verdict") != want || str(rows[i].Data, "mode") != ops.UnitCheckUncommitted {
			t.Errorf("scoped check %d: %v", i, rows[i].Data)
		}
	}
	if str(rows[3].Data, "mode") != ops.UnitCheckCommitted {
		t.Errorf("the last check of A is not of its whole change: %v", rows[3].Data)
	}
	if !(rows[0].Seq < fix.Seq && fix.Seq < rows[1].Seq && rows[1].Seq < commits[0].Seq && commits[0].Seq < s2.Seq &&
		s2.Seq < rows[2].Seq && rows[2].Seq < commits[1].Seq && commits[1].Seq < rows[3].Seq) {
		t.Errorf("checks %d,%d,%d,%d, fix %d, s2 %d, commits %d,%d: the fix and the second sub-task are not inside the first's scoped checks",
			rows[0].Seq, rows[1].Seq, rows[2].Seq, rows[3].Seq, fix.Seq, s2.Seq, commits[0].Seq, commits[1].Seq)
	}
	accepts := rowsOfNode(l, ops.OpAccept, "A")
	if len(accepts) != 1 || str(accepts[0].Data, "reviewer_call") != rows[3].CallID {
		t.Errorf("the acceptance's reviewer call is not the committed check: %v", accepts)
	}
}

// TestScopedNoAnswerStopsBeforeTheCheckpoint: a scoped check that answers nothing (the tool was not given the scope) holds
// the node before the sub-task's commit and before the next sub-task starts.
func TestScopedNoAnswerStopsBeforeTheCheckpoint(t *testing.T) {
	l := splitLabWith(t, splitCriteria, "one", "two")
	l.UnitChecks("A", UnitCheck{State: reviewadapter.StateNoAnswer})
	l.Sequence("^build:A:executor$", writes("one", "ready_for_review"))
	got := mustRun(t, l)
	if got.State == "closed" {
		t.Fatalf("the run closed with a scoped check that answered nothing: %s", got.State)
	}
	if commits := rowsOfNode(l, ops.OpCommit, "A"); len(commits) != 0 {
		t.Errorf("%d commits after a scoped no_answer", len(commits))
	}
	for _, lb := range l.Labels() {
		if strings.Contains(lb, "subtask") {
			t.Errorf("the second sub-task ran past a scoped no_answer: %v", l.Labels())
		}
	}
}
