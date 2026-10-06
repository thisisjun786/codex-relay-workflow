package hook

import (
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/goalplan"
)

// ActiveWorkPhaseOpts is the fail-open reader the D1b directive assembly takes its
// active work phase from (CXC v0.2.40 hook.ts:373-385). It reads the bound goalplan,
// resolves the effective cursor, and answers nil when there is nothing to name.
func TestActiveWorkPhaseOptsReadsTheBoundPlan(t *testing.T) {
	blocked := "vendor release"
	build := func(cursor *string, phases ...goalplan.GoalplanWorkPhase) *goalplan.Goalplan {
		p := goalplan.BuildGoalplan(goalplan.NewGoalplanInput{Objective: "demo", Now: func() string { return "2026-01-01T00:00:00.000Z" }})
		p.ActiveWorkPhaseID = cursor
		p.WorkPhases = phases
		return p
	}
	phase := func(id, title string, status goalplan.WorkPhaseStatus) goalplan.GoalplanWorkPhase {
		return goalplan.GoalplanWorkPhase{ID: id, Title: title, Status: status, Tasks: []goalplan.GoalplanTask{}, CriteriaIDs: []string{}}
	}
	write := func(t *testing.T, p *goalplan.Goalplan) string {
		t.Helper()
		cwd := t.TempDir()
		if err := goalplan.WriteGoalplan(cwd, p); err != nil {
			t.Fatal(err)
		}
		return cwd
	}

	t.Run("empty slug answers nil without reading", func(t *testing.T) {
		if got := ActiveWorkPhaseOpts(t.TempDir(), ""); got != nil {
			t.Fatalf("empty slug: %+v", got)
		}
	})

	t.Run("a missing plan answers nil", func(t *testing.T) {
		if got := ActiveWorkPhaseOpts(t.TempDir(), "no-such-plan"); got != nil {
			t.Fatalf("missing plan: %+v", got)
		}
	})

	t.Run("the first runnable phase is named", func(t *testing.T) {
		cwd := write(t, build(nil, phase("wp1", "first", goalplan.WorkPhasePending)))
		got := ActiveWorkPhaseOpts(cwd, "demo")
		if got == nil || got.ActiveWorkPhase == nil || got.ActiveWorkPhase.ID != "wp1" || got.ActiveWorkPhase.Title != "first" {
			t.Fatalf("bound plan: %+v", got)
		}
	})

	t.Run("a stale cursor falls through to the next runnable phase", func(t *testing.T) {
		cursor := "wp1"
		cwd := write(t, build(&cursor, phase("wp1", "first", goalplan.WorkPhaseBlocked), phase("wp2", "second", goalplan.WorkPhasePending)))
		got := ActiveWorkPhaseOpts(cwd, "demo")
		if got == nil || got.ActiveWorkPhase == nil || got.ActiveWorkPhase.ID != "wp2" {
			t.Fatalf("stale cursor: %+v", got)
		}
	})

	t.Run("no open phase answers nil", func(t *testing.T) {
		cursor := "wp1"
		p := build(&cursor, phase("wp1", "first", goalplan.WorkPhaseBlocked))
		p.WorkPhases[0].BlockedReason = &blocked
		cwd := write(t, p)
		if got := ActiveWorkPhaseOpts(cwd, "demo"); got != nil {
			t.Fatalf("no open phase: %+v", got)
		}
	})
}
